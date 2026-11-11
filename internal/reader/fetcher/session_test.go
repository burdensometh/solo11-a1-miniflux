// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fetcher // import "miniflux.app/v2/internal/reader/fetcher"

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// observedRequest captures what the server saw, for later assertions.
type observedRequest struct {
	path       string
	cookies    map[string]string
	remoteAddr string
}

func observe(r *http.Request) observedRequest {
	cookies := make(map[string]string, len(r.Cookies()))
	for _, cookie := range r.Cookies() {
		cookies[cookie.Name] = cookie.Value
	}

	return observedRequest{path: r.URL.Path, cookies: cookies, remoteAddr: r.RemoteAddr}
}

func executeAndDrain(t *testing.T, builder *RequestBuilder, requestURL string) {
	t.Helper()

	resp, err := builder.ExecuteRequest(requestURL)
	if err != nil {
		t.Fatalf("Expected no error for %q, got %v", requestURL, err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("Unable to read response body of %q: %v", requestURL, err)
	}
}

// Two subscriptions of the same website, each with its own cookie, must never
// see each other's credentials, even though their requests reuse the same
// pooled connections.
func TestSessionIsolationAcrossSubscriptions(t *testing.T) {
	var mu sync.Mutex
	var observed []observedRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		observed = append(observed, observe(r))
		mu.Unlock()

		switch r.URL.Path {
		case "/feedA":
			w.Header().Set("Set-Cookie", "server=fromA")
		case "/feedB":
			w.Header().Set("Set-Cookie", "server=fromB")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	subscriptionA := NewRequestBuilder().WithCookie("token=A", server.URL+"/feedA")
	subscriptionB := NewRequestBuilder().WithCookie("token=B", server.URL+"/feedB")

	// Interleave fetches of both subscriptions. The second round must carry
	// the server-set cookie of the same subscription, and only that one.
	executeAndDrain(t, subscriptionA, server.URL+"/feedA")
	executeAndDrain(t, subscriptionB, server.URL+"/feedB")
	executeAndDrain(t, subscriptionA, server.URL+"/feedA")
	executeAndDrain(t, subscriptionB, server.URL+"/feedB")

	mu.Lock()
	defer mu.Unlock()

	if len(observed) != 4 {
		t.Fatalf("Expected 4 requests, got %d", len(observed))
	}

	assertCookies := func(index int, want map[string]string, unwanted ...string) {
		t.Helper()

		got := observed[index].cookies
		for name, value := range want {
			if got[name] != value {
				t.Errorf("Request #%d (%s): expected cookie %q=%q, got %q (all cookies: %v)", index+1, observed[index].path, name, value, got[name], got)
			}
		}
		for _, name := range unwanted {
			if _, found := got[name]; found {
				t.Errorf("Request #%d (%s): cookie %q leaked from the other subscription (all cookies: %v)", index+1, observed[index].path, name, got)
			}
		}
	}

	assertCookies(0, map[string]string{"token": "A"}, "server")
	assertCookies(1, map[string]string{"token": "B"}, "server") // B must not see the cookie set for A.
	assertCookies(2, map[string]string{"token": "A", "server": "fromA"})
	assertCookies(3, map[string]string{"token": "B", "server": "fromB"})

	// The requests above ran over a shared connection pool: with keep-alive,
	// sequential requests to the same host reuse the same TCP connection.
	// Isolation therefore holds even when both subscriptions share a wire.
	connections := make(map[string]struct{})
	for _, req := range observed {
		connections[req.remoteAddr] = struct{}{}
	}
	if len(connections) > 2 {
		t.Errorf("Expected requests to reuse pooled connections, got %d distinct connections for 4 requests", len(connections))
	}
}

// Concurrent refreshes of two subscriptions of the same website must not
// exchange cookies either.
func TestSessionConcurrentSubscriptionsIsolation(t *testing.T) {
	violations := make(chan string, 64)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wantToken, unwantedServerCookie string
		switch r.URL.Path {
		case "/feedA":
			wantToken, unwantedServerCookie = "A", "fromB"
			w.Header().Set("Set-Cookie", "server=fromA")
		case "/feedB":
			wantToken, unwantedServerCookie = "B", "fromA"
			w.Header().Set("Set-Cookie", "server=fromB")
		default:
			return
		}

		if got := r.Header.Get("Cookie"); got != "" {
			cookies := observe(r).cookies
			if cookies["token"] != wantToken {
				violations <- fmt.Sprintf("%s received token=%q instead of %q", r.URL.Path, cookies["token"], wantToken)
			}
			if _, found := cookies["server"]; found && cookies["server"] == unwantedServerCookie {
				violations <- fmt.Sprintf("%s received the other subscription's cookie server=%q", r.URL.Path, cookies["server"])
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	subscriptionA := NewRequestBuilder().WithCookie("token=A", server.URL+"/feedA")
	subscriptionB := NewRequestBuilder().WithCookie("token=B", server.URL+"/feedB")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			builder, path := subscriptionA, "/feedA"
			if i%2 == 1 {
				builder, path = subscriptionB, "/feedB"
			}

			for j := 0; j < 10; j++ {
				resp, err := builder.ExecuteRequest(server.URL + path)
				if err != nil {
					violations <- fmt.Sprintf("unexpected error: %v", err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}(i)
	}
	wg.Wait()
	close(violations)

	for violation := range violations {
		t.Error(violation)
	}
}

// Within one subscription, a redirect chain must keep the session: the
// configured cookie follows the redirects, and cookies set by intermediate
// hops are sent to the next ones.
func TestSessionRedirectChainKeepsSessionCookies(t *testing.T) {
	finalCookies := make(chan map[string]string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "hop=one")
		http.Redirect(w, r, "/hop", http.StatusFound)
	})
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		cookies := observe(r).cookies
		if cookies["token"] != "A" || cookies["hop"] != "one" {
			t.Errorf("Redirect hop lost session cookies, got %v", cookies)
		}
		w.Header().Set("Set-Cookie", "hop2=two")
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		finalCookies <- observe(r).cookies
		w.WriteHeader(http.StatusOK)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	builder := NewRequestBuilder().WithCookie("token=A", server.URL+"/feed")
	executeAndDrain(t, builder, server.URL+"/feed")

	cookies := <-finalCookies
	for name, value := range map[string]string{"token": "A", "hop": "one", "hop2": "two"} {
		if cookies[name] != value {
			t.Errorf("Expected final request to carry cookie %q=%q, got %q (all cookies: %v)", name, value, cookies[name], cookies)
		}
	}
}

// A redirect to a different website must not carry the subscription cookies.
func TestSessionRedirectToAnotherWebsiteDropsCookies(t *testing.T) {
	finalCookies := make(chan map[string]string, 1)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Unable to parse server URL: %v", err)
	}

	// "localhost" and "127.0.0.1" are different websites even though they
	// resolve to the same address here.
	otherWebsiteURL := fmt.Sprintf("http://localhost:%s/final", serverURL.Port())

	mux.HandleFunc("/feed", func(w http.ResponseWriter, r *http.Request) {
		if got := observe(r).cookies["token"]; got != "A" {
			t.Errorf("Expected the initial request to carry the configured cookie, got %q", got)
		}
		http.Redirect(w, r, otherWebsiteURL, http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		finalCookies <- observe(r).cookies
		w.WriteHeader(http.StatusOK)
	})

	builder := NewRequestBuilder().WithCookie("token=A", server.URL+"/feed")
	executeAndDrain(t, builder, server.URL+"/feed")

	if cookies := <-finalCookies; len(cookies) != 0 {
		t.Errorf("Expected no cookies on the other website, got %v", cookies)
	}
}

// Cookies of an internationalized domain name must be sent whether the
// request URL uses the Unicode or the punycode form of the domain.
func TestSessionInternationalizedDomainCookie(t *testing.T) {
	receivedCookies := make(chan string, 2)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCookies <- r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	}))
	defer proxyServer.Close()

	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse proxy URL: %v", err)
	}

	builder := NewRequestBuilder().
		WithCustomApplicationProxyURL(proxyURL).
		UseCustomApplicationProxyURL(true).
		WithCookie("session=secret", "https://www.bücher.example/feed")

	requests := []struct {
		requestURL string
		expected   string
	}{
		{"http://xn--bcher-kva.example/rss.xml", "session=secret"},
		{"http://www.xn--bcher-kva.example/rss.xml", "session=secret"},
	}

	for _, tt := range requests {
		resp, err := builder.ExecuteRequest(tt.requestURL)
		if err != nil {
			t.Fatalf("Expected no error for %q, got %v", tt.requestURL, err)
		}
		resp.Body.Close()

		if cookie := <-receivedCookies; cookie != tt.expected {
			t.Errorf("Expected Cookie for %q to be %q, got %q", tt.requestURL, tt.expected, cookie)
		}
	}
}

// A clone belongs to the same session as the original builder: cookies set by
// the server on one are visible to the other. This is what allows the
// subscription discovery to probe several URLs of the same website.
func TestSessionCloneSharesSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "server=seen")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	builder := NewRequestBuilder().WithCookie("token=A", server.URL+"/feed")
	executeAndDrain(t, builder, server.URL+"/feed")

	secondRequestCookies := make(chan map[string]string, 1)
	echoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondRequestCookies <- observe(r).cookies
		w.WriteHeader(http.StatusOK)
	}))
	defer echoServer.Close()

	// Both servers are the same website (127.0.0.1), so the session applies.
	echoURL, err := url.Parse(echoServer.URL)
	if err != nil {
		t.Fatalf("Unable to parse server URL: %v", err)
	}

	executeAndDrain(t, builder.Clone().WithoutRedirects(), echoURL.String())

	cookies := <-secondRequestCookies
	if cookies["token"] != "A" || cookies["server"] != "seen" {
		t.Errorf("Expected the clone to share the session of the original, got %v", cookies)
	}
}
