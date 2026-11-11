// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/model"
)

// allowPrivateNetworks lets the tests reach httptest servers bound to the
// loopback address, mirroring internal/http/client tests.
func allowPrivateNetworks(t *testing.T) {
	t.Helper()
	t.Setenv("INTEGRATION_ALLOW_PRIVATE_NETWORKS", "1")

	parsedOptions, err := config.NewConfigParser().ParseEnvironmentVariables()
	if err != nil {
		t.Fatalf("unable to configure test options: %v", err)
	}

	previousOptions := config.Opts
	config.Opts = parsedOptions
	t.Cleanup(func() {
		config.Opts = previousOptions
	})
}

func newTestClient(t *testing.T, webhookURL string) *Client {
	t.Helper()
	allowPrivateNetworks(t)
	c := NewClient(webhookURL, "test-secret")
	c.maxAttempts = 3
	c.initialBackoff = time.Second
	c.maxBackoff = 30 * time.Second
	return c
}

type receivedRequest struct {
	deliveryIDHeader string
	eventTypeHeader  string
	signatureHeader  string
	body             []byte
}

func recordServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func() []receivedRequest) {
	t.Helper()
	var mu sync.Mutex
	var received []receivedRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("unable to read request body: %v", err)
		}
		mu.Lock()
		received = append(received, receivedRequest{
			deliveryIDHeader: r.Header.Get(DeliveryIDHeader),
			eventTypeHeader:  r.Header.Get("X-Miniflux-Event-Type"),
			signatureHeader:  r.Header.Get("X-Miniflux-Signature"),
			body:             body,
		})
		mu.Unlock()
		handler(w, r)
	}))

	return server, func() []receivedRequest {
		mu.Lock()
		defer mu.Unlock()
		out := make([]receivedRequest, len(received))
		copy(out, received)
		return out
	}
}

func statusHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}
}

func TestSendNewEntriesSuccess(t *testing.T) {
	server, getReceived := recordServer(t, statusHandler(http.StatusOK))
	defer server.Close()

	c := newTestClient(t, server.URL)
	var slept []time.Duration
	c.sleep = func(d time.Duration) { slept = append(slept, d) }

	feed := &model.Feed{ID: 1, Category: &model.Category{ID: 2}}
	entries := model.Entries{{ID: 10, FeedID: 1}}
	if err := c.SendNewEntriesWebhookEvent(feed, entries); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	received := getReceived()
	if len(received) != 1 {
		t.Fatalf("expected exactly 1 request, got %d", len(received))
	}
	if len(slept) != 0 {
		t.Fatalf("expected no retry delay, got %v", slept)
	}

	req := received[0]
	if req.deliveryIDHeader == "" {
		t.Fatal("expected a delivery ID header")
	}
	if req.eventTypeHeader != NewEntriesEventType {
		t.Fatalf("expected event type %q, got %q", NewEntriesEventType, req.eventTypeHeader)
	}
	if req.signatureHeader == "" {
		t.Fatal("expected a signature header")
	}

	var payload struct {
		EventType  string `json:"event_type"`
		DeliveryID string `json:"delivery_id"`
	}
	if err := json.Unmarshal(req.body, &payload); err != nil {
		t.Fatalf("unable to decode payload: %v", err)
	}
	if payload.DeliveryID != req.deliveryIDHeader {
		t.Fatalf("delivery ID in body %q does not match header %q", payload.DeliveryID, req.deliveryIDHeader)
	}
	if payload.EventType != NewEntriesEventType {
		t.Fatalf("expected event type %q in body, got %q", NewEntriesEventType, payload.EventType)
	}
}

func TestRetryOn502ThenSuccess(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()
		if current < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
	server, getReceived := recordServer(t, handler)
	defer server.Close()

	c := newTestClient(t, server.URL)
	var slept []time.Duration
	c.sleep = func(d time.Duration) { slept = append(slept, d) }

	feed := &model.Feed{ID: 1, Category: &model.Category{ID: 2}}
	if err := c.SendNewEntriesWebhookEvent(feed, model.Entries{{ID: 10}}); err != nil {
		t.Fatalf("expected delivery to eventually succeed, got %v", err)
	}

	received := getReceived()
	if len(received) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(received))
	}

	// Every attempt must carry the same delivery ID and an identical body so
	// the receiver can recognize and deduplicate the retries.
	first := received[0]
	for i, req := range received {
		if req.deliveryIDHeader != first.deliveryIDHeader {
			t.Fatalf("attempt %d changed delivery ID: %q != %q", i+1, req.deliveryIDHeader, first.deliveryIDHeader)
		}
		if string(req.body) != string(first.body) {
			t.Fatalf("attempt %d sent a different body", i+1)
		}
		if req.signatureHeader != first.signatureHeader {
			t.Fatalf("attempt %d changed the signature", i+1)
		}
	}

	if len(slept) != 2 {
		t.Fatalf("expected 2 backoff delays, got %d", len(slept))
	}
	// Full jitter: first delay within [0, 1s], second within [0, 2s].
	if slept[0] < 0 || slept[0] > time.Second {
		t.Fatalf("first backoff %v outside [0, 1s]", slept[0])
	}
	if slept[1] < 0 || slept[1] > 2*time.Second {
		t.Fatalf("second backoff %v outside [0, 2s]", slept[1])
	}
}

func TestRetryExhaustedOnPersistent502(t *testing.T) {
	server, getReceived := recordServer(t, statusHandler(http.StatusBadGateway))
	defer server.Close()

	c := newTestClient(t, server.URL)
	var slept []time.Duration
	c.sleep = func(d time.Duration) { slept = append(slept, d) }

	err := c.SendNewEntriesWebhookEvent(
		&model.Feed{ID: 1, Category: &model.Category{ID: 2}},
		model.Entries{{ID: 10}},
	)
	if err == nil {
		t.Fatal("expected an error when all attempts fail")
	}

	received := getReceived()
	if len(received) != c.maxAttempts {
		t.Fatalf("expected %d attempts, got %d", c.maxAttempts, len(received))
	}
	if len(slept) != c.maxAttempts-1 {
		t.Fatalf("expected %d backoff delays, got %d", c.maxAttempts-1, len(slept))
	}
}

func TestNoRetryOnPermanentStatus(t *testing.T) {
	server, getReceived := recordServer(t, statusHandler(http.StatusNotFound))
	defer server.Close()

	c := newTestClient(t, server.URL)
	sleepCalled := false
	c.sleep = func(d time.Duration) { sleepCalled = true }

	err := c.SendNewEntriesWebhookEvent(
		&model.Feed{ID: 1, Category: &model.Category{ID: 2}},
		model.Entries{{ID: 10}},
	)
	if err == nil {
		t.Fatal("expected an error for status 404")
	}

	if len(getReceived()) != 1 {
		t.Fatalf("expected a single attempt without retries, got %d", len(getReceived()))
	}
	if sleepCalled {
		t.Fatal("permanent errors must not be retried")
	}
}

func TestRetryOnTransportError(t *testing.T) {
	server := httptest.NewServer(statusHandler(http.StatusOK))
	// Closing the server makes every connection attempt fail with a
	// transport-level error, which must be retried.
	server.Close()

	c := newTestClient(t, server.URL)
	var slept []time.Duration
	c.sleep = func(d time.Duration) { slept = append(slept, d) }

	err := c.SendNewEntriesWebhookEvent(
		&model.Feed{ID: 1, Category: &model.Category{ID: 2}},
		model.Entries{{ID: 10}},
	)
	if err == nil {
		t.Fatal("expected an error when the endpoint is unreachable")
	}
	if len(slept) != c.maxAttempts-1 {
		t.Fatalf("expected transport errors to be retried %d times, got %d delays", c.maxAttempts-1, len(slept))
	}
}

func TestBackoffDelayUsesFullJitterAndCapping(t *testing.T) {
	c := newTestClient(t, "http://localhost")
	c.initialBackoff = 100 * time.Millisecond
	c.maxBackoff = 400 * time.Millisecond

	bounds := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond}
	for attempt, upper := range bounds {
		for range 100 {
			delay := c.backoffDelay(attempt + 1)
			if delay < 0 || delay > upper {
				t.Fatalf("attempt %d: backoff %v outside [0, %v]", attempt+1, delay, upper)
			}
		}
	}
}

func TestEmptyEntriesMakesNoRequest(t *testing.T) {
	server, getReceived := recordServer(t, statusHandler(http.StatusOK))
	defer server.Close()

	c := newTestClient(t, server.URL)
	if err := c.SendNewEntriesWebhookEvent(&model.Feed{}, model.Entries{}); err != nil {
		t.Fatalf("expected no error for empty entries, got %v", err)
	}
	if len(getReceived()) != 0 {
		t.Fatal("expected no HTTP request for an empty batch")
	}
}

func TestSaveEntryEventCarriesDeliveryID(t *testing.T) {
	server, getReceived := recordServer(t, statusHandler(http.StatusOK))
	defer server.Close()

	c := newTestClient(t, server.URL)
	c.sleep = func(time.Duration) {}

	entry := &model.Entry{
		ID:     42,
		UserID: 7,
		Feed:   &model.Feed{ID: 1, Category: &model.Category{ID: 2}},
	}
	if err := c.SendSaveEntryWebhookEvent(entry); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	received := getReceived()
	if len(received) != 1 {
		t.Fatalf("expected 1 request, got %d", len(received))
	}
	if received[0].eventTypeHeader != SaveEntryEventType {
		t.Fatalf("expected event type %q, got %q", SaveEntryEventType, received[0].eventTypeHeader)
	}

	var payload struct {
		DeliveryID string `json:"delivery_id"`
	}
	if err := json.Unmarshal(received[0].body, &payload); err != nil {
		t.Fatalf("unable to decode payload: %v", err)
	}
	if payload.DeliveryID == "" {
		t.Fatal("expected a delivery ID in the save entry payload")
	}
	if payload.DeliveryID != received[0].deliveryIDHeader {
		t.Fatal("delivery ID in body and header do not match")
	}
}

func TestMissingWebhookURL(t *testing.T) {
	c := NewClient("", "secret")
	if err := c.SendNewEntriesWebhookEvent(
		&model.Feed{ID: 1, Category: &model.Category{ID: 2}},
		model.Entries{{ID: 10}},
	); err == nil {
		t.Fatal("expected an error when the webhook URL is missing")
	}
}
