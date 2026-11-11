// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fetcher // import "miniflux.app/v2/internal/reader/fetcher"

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/proxyrotator"
	"miniflux.app/v2/internal/urllib"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

const (
	defaultHTTPClientTimeout = 20 * time.Second
	defaultAcceptHeader      = "application/xml,application/atom+xml,application/rss+xml,application/rdf+xml,application/feed+json,text/html,*/*;q=0.9"
)

var (
	ErrHostnameResolution = errors.New("fetcher: unable to resolve request hostname")
	ErrPrivateNetworkHost = errors.New("fetcher: refusing to access private network host")
)

type proxySource int

const (
	proxySourceNone proxySource = iota
	proxySourceFeed
	proxySourceApplication
	proxySourceRotator
)

type RequestBuilder struct {
	headers            http.Header
	jar                *cookiejar.Jar
	clientProxyURL     *url.URL
	clientTimeout      time.Duration
	useClientProxy     bool
	withoutRedirects   bool
	ignoreTLSErrors    bool
	disableHTTP2       bool
	disableCompression bool
	proxyRotator       *proxyrotator.ProxyRotator
	feedProxyURL       string
}

func NewRequestBuilder() *RequestBuilder {
	// The jar is scoped to the public suffix list so that a server cannot set
	// cookies for an entire public suffix (for example "co.uk").
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: cookieJarPublicSuffixList{}})
	if err != nil {
		// cookiejar.New only fails on a nil option list; it cannot happen here.
		panic(fmt.Sprintf("fetcher: unable to create cookie jar: %v", err))
	}

	return &RequestBuilder{
		headers:       make(http.Header),
		jar:           jar,
		clientTimeout: defaultHTTPClientTimeout,
	}
}

// cookieJarPublicSuffixList adapts golang.org/x/net/publicsuffix to the
// cookiejar.PublicSuffixList interface.
type cookieJarPublicSuffixList struct{}

func (cookieJarPublicSuffixList) PublicSuffix(domain string) string {
	suffix, _ := publicsuffix.PublicSuffix(domain)
	return suffix
}

func (cookieJarPublicSuffixList) String() string {
	return "miniflux fetcher (golang.org/x/net/publicsuffix)"
}

// Clone returns an independent copy of the builder. Mutating the copy (for
// example to disable redirects for a single request) leaves the original
// untouched. The clone shares the cookie jar of the original: both belong to
// the same subscription session.
func (r *RequestBuilder) Clone() *RequestBuilder {
	clone := *r
	clone.headers = r.headers.Clone()
	return &clone
}

func (r *RequestBuilder) WithHeader(key, value string) *RequestBuilder {
	r.headers.Set(key, value)
	return r
}

func (r *RequestBuilder) WithETag(etag string) *RequestBuilder {
	if etag != "" {
		r.headers.Set("If-None-Match", etag)
	}
	return r
}

func (r *RequestBuilder) WithLastModified(lastModified string) *RequestBuilder {
	if lastModified != "" {
		r.headers.Set("If-Modified-Since", lastModified)
	}
	return r
}

func (r *RequestBuilder) WithUserAgent(userAgent string, defaultUserAgent string) *RequestBuilder {
	if userAgent != "" {
		r.headers.Set("User-Agent", userAgent)
	} else {
		r.headers.Set("User-Agent", defaultUserAgent)
	}
	return r
}

// WithCookie seeds the session cookie jar with the cookies of the website of
// siteURL, that is its host or any host sharing its registrable domain. A
// cookie configured for a feed must not be sent to the other websites it
// links to.
//
// The cookie value must be a valid Cookie header ("name=value; ...") as
// defined by RFC 6265; pairs that are not valid cookies are ignored. Seeded
// cookies live in the builder's own jar, which is never shared across
// subscriptions: two feeds of the same website with different cookies cannot
// see each other's cookies, even though their requests may reuse the same
// pooled connections. Cookies set by the server during a redirect chain are
// stored in the same jar and sent on the next hops.
func (r *RequestBuilder) WithCookie(cookie, siteURL string) *RequestBuilder {
	if cookie == "" {
		return r
	}

	parsedSiteURL, err := url.Parse(siteURL)
	if err != nil || parsedSiteURL.Hostname() == "" {
		return r
	}

	// The jar only accepts http(s) seed URLs. The scheme does not matter
	// otherwise: seeded cookies are not marked Secure, so they are sent over
	// both HTTP and HTTPS, like a static Cookie header would be.
	seedURL := &url.URL{Scheme: "https", Host: parsedSiteURL.Host, Path: "/"}

	cookies := parseCookieHeaderValue(cookie)

	// Scope the cookies to the registrable domain (eTLD+1) of the website, so
	// that "feeds.example.org" and "www.example.org" share them while
	// "example.net" never sees them. Hosts without a registrable domain (IP
	// addresses, "localhost") get host-only cookies instead.
	if domain, err := registrableDomain(parsedSiteURL.Hostname()); err == nil {
		for _, c := range cookies {
			c.Domain = domain
		}
	}

	r.jar.SetCookies(seedURL, cookies)
	return r
}

func (r *RequestBuilder) WithUsernameAndPassword(username, password string) *RequestBuilder {
	if username != "" && password != "" {
		r.headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	}
	return r
}

func (r *RequestBuilder) WithProxyRotator(proxyRotator *proxyrotator.ProxyRotator) *RequestBuilder {
	r.proxyRotator = proxyRotator
	return r
}

func (r *RequestBuilder) WithCustomApplicationProxyURL(proxyURL *url.URL) *RequestBuilder {
	r.clientProxyURL = proxyURL
	return r
}

func (r *RequestBuilder) UseCustomApplicationProxyURL(value bool) *RequestBuilder {
	r.useClientProxy = value
	return r
}

func (r *RequestBuilder) WithCustomFeedProxyURL(proxyURL string) *RequestBuilder {
	r.feedProxyURL = proxyURL
	return r
}

func (r *RequestBuilder) WithTimeout(timeout time.Duration) *RequestBuilder {
	r.clientTimeout = timeout
	return r
}

func (r *RequestBuilder) WithoutRedirects() *RequestBuilder {
	r.withoutRedirects = true
	return r
}

func (r *RequestBuilder) DisableHTTP2(value bool) *RequestBuilder {
	r.disableHTTP2 = value
	return r
}

func (r *RequestBuilder) IgnoreTLSErrors(value bool) *RequestBuilder {
	r.ignoreTLSErrors = value
	return r
}

func (r *RequestBuilder) WithoutCompression() *RequestBuilder {
	r.disableCompression = true
	return r
}

func (r *RequestBuilder) ExecuteRequest(requestURL string) (*http.Response, error) {
	var clientProxyURL *url.URL
	proxySource := proxySourceNone

	switch {
	case r.feedProxyURL != "":
		var err error
		clientProxyURL, err = url.Parse(r.feedProxyURL)
		if err != nil {
			return nil, fmt.Errorf(`fetcher: invalid feed proxy URL %q: %w`, r.feedProxyURL, err)
		}
		proxySource = proxySourceFeed
	case r.useClientProxy && r.clientProxyURL != nil:
		clientProxyURL = r.clientProxyURL
		proxySource = proxySourceApplication
	case r.proxyRotator != nil && r.proxyRotator.HasProxies():
		clientProxyURL = r.proxyRotator.GetNextProxy()
		proxySource = proxySourceRotator
	}

	key := transportKey{
		proxySource:           proxySource,
		ignoreTLSErrors:       r.ignoreTLSErrors,
		disableHTTP2:          r.disableHTTP2,
		allowPrivateNetworks:  config.Opts == nil || config.Opts.FetcherAllowPrivateNetworks(),
		allowPrivateFeedProxy: config.Opts != nil && config.Opts.FetcherAllowPrivateFeedProxy(),
		proxyEnvironment:      proxyEnvironmentFingerprint(),
	}
	if clientProxyURL != nil {
		key.proxyURL = clientProxyURL.String()
	}

	var clientProxyURLRedacted string
	if clientProxyURL != nil {
		clientProxyURLRedacted = clientProxyURL.Redacted()
	}

	client := &http.Client{
		Transport: sharedTransport(key),
		Timeout:   r.clientTimeout,
		Jar:       r.jar,
	}

	if r.withoutRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header = r.headers.Clone()

	if r.disableCompression {
		req.Header.Set("Accept-Encoding", "identity")
	} else {
		req.Header.Set("Accept-Encoding", "br,gzip")
	}

	// Set default Accept header if not already set.
	// Note that for the media proxy requests, we need to forward the browser Accept header.
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", defaultAcceptHeader)
	}

	slog.Debug("Making outgoing request", slog.Group("request",
		slog.String("method", req.Method),
		slog.String("url", req.URL.String()),
		slog.Any("headers", req.Header),
		slog.Bool("without_redirects", r.withoutRedirects),
		slog.Bool("use_app_client_proxy", r.useClientProxy),
		slog.String("client_proxy_url", clientProxyURLRedacted),
		slog.Bool("ignore_tls_errors", r.ignoreTLSErrors),
		slog.Bool("disable_http2", r.disableHTTP2),
	))

	return client.Do(req)
}

// transportKey identifies a connection pool. Two requests share a pool only
// when their network path is identical: same proxy, same TLS and protocol
// options, same private-network policy, and same proxy environment. Session
// state (cookies) is never part of the key: it lives in the per-builder
// cookie jar, so subscriptions sharing a pool cannot see each other's
// credentials.
type transportKey struct {
	proxyURL              string
	proxySource           proxySource
	ignoreTLSErrors       bool
	disableHTTP2          bool
	allowPrivateNetworks  bool
	allowPrivateFeedProxy bool
	proxyEnvironment      string
}

// sharedTransports pools the transports of all requests with the same
// transportKey. The number of pools is bounded by the number of distinct
// network configurations, not by the number of subscriptions or websites:
// http.Transport already maintains per-host connection pools internally.
var sharedTransports sync.Map // map[transportKey]*http.Transport

func sharedTransport(key transportKey) *http.Transport {
	if transport, ok := sharedTransports.Load(key); ok {
		return transport.(*http.Transport)
	}

	transport := newTransport(key)
	actual, _ := sharedTransports.LoadOrStore(key, transport)
	return actual.(*http.Transport)
}

func newTransport(key transportKey) *http.Transport {
	var clientProxyURL *url.URL
	if key.proxyURL != "" {
		// The URL was already validated by ExecuteRequest.
		clientProxyURL, _ = url.Parse(key.proxyURL)
	}

	directDialer := &net.Dialer{
		Timeout:   10 * time.Second, // Default is 30s.
		KeepAlive: 15 * time.Second, // Default is 30s.
	}

	proxyDialer := &net.Dialer{
		Timeout:   10 * time.Second, // Default is 30s.
		KeepAlive: 15 * time.Second, // Default is 30s.
	}

	// The proxy is resolved with httpproxy rather than http.ProxyFromEnvironment,
	// which caches the environment on first use. Routing and the trusted hop below
	// are then decided by the same call. The environment is part of the transport
	// key, so a pooled transport never sees a stale proxy configuration.
	envProxyFunc := httpproxy.FromEnvironment().ProxyFunc()

	trustedProxies := &trustedProxyAddresses{}
	if key.proxySource != proxySourceFeed || key.allowPrivateFeedProxy {
		trustedProxies.add(clientProxyURL)
	}

	// Perform the private-network check inside the dialer's Control callback,
	// which fires after DNS resolution but before the TCP connection is made.
	// This eliminates TOCTOU / DNS-rebinding vulnerabilities: the resolved IP
	// that is checked is exactly the IP that will be connected to.
	if !key.allowPrivateNetworks {
		directDialer.Control = func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}

			ip := net.ParseIP(host)
			if urllib.IsNonPublicIP(ip) {
				return fmt.Errorf("%w %q", ErrPrivateNetworkHost, host)
			}

			return nil
		}
	}

	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			proxyURL, err := envProxyFunc(req.URL)
			if err == nil {
				trustedProxies.add(proxyURL)
			}

			return proxyURL, err
		},
		// Setting `DialContext` disables HTTP/2, this option forces the transport to try HTTP/2 regardless.
		ForceAttemptHTTP2: true,
		MaxIdleConns:      50,               // Default is 100.
		IdleConnTimeout:   10 * time.Second, // Default is 90s.
	}

	transport.DialContext = directDialer.DialContext
	if !key.allowPrivateNetworks {
		// Explicitly configured proxies are a trusted hop. Keep the private-network
		// check for direct requests and redirects, but allow the connection to the proxy itself.
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if trustedProxies.contains(normalizeDialAddress(addr)) {
				return proxyDialer.DialContext(ctx, network, addr)
			}

			return directDialer.DialContext(ctx, network, addr)
		}
	}

	if key.ignoreTLSErrors {
		//  Add insecure ciphers if we are ignoring TLS errors. This allows to connect to badly configured servers anyway
		ciphers := slices.Concat(tls.CipherSuites(), tls.InsecureCipherSuites())
		cipherSuites := make([]uint16, 0, len(ciphers))
		for _, cipher := range ciphers {
			cipherSuites = append(cipherSuites, cipher.ID)
		}
		transport.TLSClientConfig = &tls.Config{
			CipherSuites:       cipherSuites,
			InsecureSkipVerify: true,
		}
	}

	if key.disableHTTP2 {
		transport.ForceAttemptHTTP2 = false

		// https://pkg.go.dev/net/http#hdr-HTTP_2
		// Programs that must disable HTTP/2 can do so by setting [Transport.TLSNextProto] (for clients) or [Server.TLSNextProto] (for servers) to a non-nil, empty map.
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}

	if clientProxyURL != nil {
		transport.Proxy = http.ProxyURL(clientProxyURL)
	}

	return transport
}

// proxyEnvironmentFingerprint captures the environment variables consulted by
// httpproxy.FromEnvironment, so that a change of environment (for example in
// tests) never reuses a transport built with different proxy settings.
func proxyEnvironmentFingerprint() string {
	variables := []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy", "REQUEST_METHOD"}
	values := make([]string, 0, len(variables))
	for _, name := range variables {
		values = append(values, os.Getenv(name))
	}

	return strings.Join(values, "\x00")
}

// parseCookieHeaderValue parses a Cookie header value ("name=value; ...")
// into a list of cookies. Pairs that are not valid "name=value" cookies are
// ignored.
func parseCookieHeaderValue(header string) []*http.Cookie {
	var cookies []*http.Cookie
	for _, part := range strings.Split(header, ";") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if name = strings.TrimSpace(name); !found || name == "" {
			continue
		}

		cookies = append(cookies, &http.Cookie{Name: name, Value: strings.TrimSpace(value)})
	}

	return cookies
}

// registrableDomain returns the eTLD+1 of host. The public suffix list only
// matches lowercase ASCII names, so the host is converted to that form first.
// IP addresses and single-label hosts don't have one.
func registrableDomain(host string) (string, error) {
	asciiHost, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", err
	}

	return publicsuffix.EffectiveTLDPlusOne(asciiHost)
}

func normalizeDialAddress(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}

	return net.JoinHostPort(strings.ToLower(host), port)
}

func normalizeProxyDialAddress(proxyURL *url.URL) string {
	if proxyURL == nil {
		return ""
	}

	port := proxyURL.Port()
	if port == "" {
		switch strings.ToLower(proxyURL.Scheme) {
		case "", "http":
			port = "80"
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			return ""
		}
	}

	return net.JoinHostPort(strings.ToLower(proxyURL.Hostname()), port)
}

// trustedProxyAddresses holds the dial addresses of the proxies used for a
// request. The proxy taken from the environment is resolved per request URL, so
// a redirect can select a different proxy than the initial request did.
type trustedProxyAddresses struct {
	mu        sync.Mutex
	addresses []string
}

func (p *trustedProxyAddresses) add(proxyURL *url.URL) {
	address := normalizeProxyDialAddress(proxyURL)
	if address == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if !slices.Contains(p.addresses, address) {
		p.addresses = append(p.addresses, address)
	}
}

func (p *trustedProxyAddresses) contains(address string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Contains(p.addresses, address)
}
