// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webhook // import "miniflux.app/v2/internal/integration/webhook"

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"

	"miniflux.app/v2/internal/crypto"
	"miniflux.app/v2/internal/http/client"
	"miniflux.app/v2/internal/model"
)

const (
	NewEntriesEventType = "new_entries"
	SaveEntryEventType  = "save_entry"

	// DeliveryIDHeader is the HTTP header carrying the unique identifier of a
	// webhook delivery. The same identifier is also present in the request body
	// and stays unchanged across retries, so receivers can deduplicate
	// deliveries that arrive more than once.
	DeliveryIDHeader = "X-Miniflux-Delivery-ID"

	// defaultMaxAttempts is the total number of HTTP attempts performed per
	// delivery (the initial attempt plus retries).
	defaultMaxAttempts = 4

	// defaultInitialBackoff is the base delay used for the exponential backoff
	// before the first retry. Full jitter is applied on top of it.
	defaultInitialBackoff = time.Second

	// defaultMaxBackoff caps the backoff delay between two attempts.
	defaultMaxBackoff = 30 * time.Second
)

// Client sends webhook events to a configured endpoint with retries and
// exponential backoff.
type Client struct {
	webhookURL    string
	webhookSecret string

	// maxAttempts is the total number of attempts (initial attempt + retries).
	maxAttempts int
	// initialBackoff is the base delay before the first retry.
	initialBackoff time.Duration
	// maxBackoff caps the delay between two attempts.
	maxBackoff time.Duration
	// sleep pauses for the given duration between attempts. It is overridable
	// in tests to avoid real waiting.
	sleep func(time.Duration)
}

func NewClient(webhookURL, webhookSecret string) *Client {
	return &Client{
		webhookURL:     webhookURL,
		webhookSecret:  webhookSecret,
		maxAttempts:    defaultMaxAttempts,
		initialBackoff: defaultInitialBackoff,
		maxBackoff:     defaultMaxBackoff,
		sleep:          time.Sleep,
	}
}

func (c *Client) SendSaveEntryWebhookEvent(entry *model.Entry) error {
	event := &WebhookSaveEntryEvent{
		EventType: SaveEntryEventType,
		Entry: &WebhookEntry{
			ID:          entry.ID,
			UserID:      entry.UserID,
			FeedID:      entry.FeedID,
			Status:      entry.Status,
			Hash:        entry.Hash,
			Title:       entry.Title,
			URL:         entry.URL,
			CommentsURL: entry.CommentsURL,
			Date:        entry.Date,
			CreatedAt:   entry.CreatedAt,
			ChangedAt:   entry.ChangedAt,
			Content:     entry.Content,
			Author:      entry.Author,
			ShareCode:   entry.ShareCode,
			Starred:     entry.Starred,
			ReadingTime: entry.ReadingTime,
			Enclosures:  entry.Enclosures,
			Tags:        entry.Tags,
			Feed: &WebhookFeed{
				ID:         entry.Feed.ID,
				UserID:     entry.Feed.UserID,
				CategoryID: entry.Feed.Category.ID,
				Category:   &WebhookCategory{ID: entry.Feed.Category.ID, Title: entry.Feed.Category.Title},
				FeedURL:    entry.Feed.FeedURL,
				SiteURL:    entry.Feed.SiteURL,
				Title:      entry.Feed.Title,
				CheckedAt:  entry.Feed.CheckedAt,
			},
		},
	}
	return c.makeRequest(SaveEntryEventType, event)
}

func (c *Client) SendNewEntriesWebhookEvent(feed *model.Feed, entries model.Entries) error {
	if len(entries) == 0 {
		return nil
	}

	webhookEntries := make([]*WebhookEntry, 0, len(entries))
	for _, entry := range entries {
		webhookEntries = append(webhookEntries, &WebhookEntry{
			ID:          entry.ID,
			UserID:      entry.UserID,
			FeedID:      entry.FeedID,
			Status:      entry.Status,
			Hash:        entry.Hash,
			Title:       entry.Title,
			URL:         entry.URL,
			CommentsURL: entry.CommentsURL,
			Date:        entry.Date,
			CreatedAt:   entry.CreatedAt,
			ChangedAt:   entry.ChangedAt,
			Content:     entry.Content,
			Author:      entry.Author,
			ShareCode:   entry.ShareCode,
			Starred:     entry.Starred,
			ReadingTime: entry.ReadingTime,
			Enclosures:  entry.Enclosures,
			Tags:        entry.Tags,
		})
	}
	event := &WebhookNewEntriesEvent{
		EventType: NewEntriesEventType,
		Feed: &WebhookFeed{
			ID:         feed.ID,
			UserID:     feed.UserID,
			CategoryID: feed.Category.ID,
			Category:   &WebhookCategory{ID: feed.Category.ID, Title: feed.Category.Title},
			FeedURL:    feed.FeedURL,
			SiteURL:    feed.SiteURL,
			Title:      feed.Title,
			CheckedAt:  feed.CheckedAt,
		},
		Entries: webhookEntries,
	}
	return c.makeRequest(NewEntriesEventType, event)
}

// makeRequest delivers a single webhook event. A delivery ID is generated once
// and reused on every attempt so the receiver can recognize retries of the same
// delivery and deduplicate them.
func (c *Client) makeRequest(eventType string, event deliveryEvent) error {
	if c.webhookURL == "" {
		return errors.New(`webhook: missing webhook URL`)
	}

	deliveryID := crypto.GenerateUUID()
	event.setDeliveryID(deliveryID)

	requestBody, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("webhook: unable to encode request body: %w", err)
	}

	maxAttempts := c.maxAttempts
	if maxAttempts < 1 {
		maxAttempts = defaultMaxAttempts
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		retryable, err := c.sendOnce(eventType, deliveryID, requestBody)
		if err == nil {
			return nil
		}

		if !retryable || attempt == maxAttempts {
			return err
		}

		delay := c.backoffDelay(attempt)
		slog.Warn("Webhook delivery failed, retrying",
			slog.String("webhook_url", c.webhookURL),
			slog.String("event_type", eventType),
			slog.String("delivery_id", deliveryID),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", maxAttempts),
			slog.Duration("retry_delay", delay),
			slog.Any("error", err),
		)
		c.sleep(delay)
	}

	return nil
}

// sendOnce performs a single HTTP attempt and reports whether a failed attempt
// is eligible for a retry. The request body is reused verbatim across retries,
// which keeps the signature and the delivery ID identical.
func (c *Client) sendOnce(eventType, deliveryID string, requestBody []byte) (retryable bool, err error) {
	response, err := client.NewRequestBuilder(c.webhookURL).
		WithMethod(http.MethodPost).
		WithJSONBody(requestBody).
		WithHeader("X-Miniflux-Signature", crypto.GenerateSHA256Hmac(c.webhookSecret, requestBody)).
		WithHeader("X-Miniflux-Event-Type", eventType).
		WithHeader(DeliveryIDHeader, deliveryID).
		Do()
	if err != nil {
		// Transport-level failures (connection refused, timeout, DNS error,
		// ...) are transient: the request may never have reached the receiver.
		return true, fmt.Errorf("webhook: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusBadGateway {
		// A 502 means the whole batch was not processed and must be redelivered.
		return true, fmt.Errorf("webhook: incorrect response status code %d for url %s", response.StatusCode, c.webhookURL)
	}

	if response.StatusCode >= 400 {
		// Client errors and other permanent server errors are not retried.
		return false, fmt.Errorf("webhook: incorrect response status code %d for url %s", response.StatusCode, c.webhookURL)
	}

	return false, nil
}

// backoffDelay computes the delay before the retry following the given attempt
// number (1-based). It uses exponential backoff with full jitter to spread
// retries when multiple deliveries fail at the same time.
func (c *Client) backoffDelay(attempt int) time.Duration {
	initialBackoff := c.initialBackoff
	if initialBackoff <= 0 {
		initialBackoff = defaultInitialBackoff
	}
	maxBackoff := c.maxBackoff
	if maxBackoff <= 0 {
		maxBackoff = defaultMaxBackoff
	}

	upper := initialBackoff << (attempt - 1)
	if upper <= 0 || upper > maxBackoff {
		upper = maxBackoff
	}
	return time.Duration(rand.Int64N(int64(upper) + 1))
}

// deliveryEvent is implemented by every webhook event payload so the client can
// attach the delivery ID before sending.
type deliveryEvent interface {
	setDeliveryID(deliveryID string)
}

type WebhookFeed struct {
	ID         int64            `json:"id"`
	UserID     int64            `json:"user_id"`
	CategoryID int64            `json:"category_id"`
	Category   *WebhookCategory `json:"category,omitempty"`
	FeedURL    string           `json:"feed_url"`
	SiteURL    string           `json:"site_url"`
	Title      string           `json:"title"`
	CheckedAt  time.Time        `json:"checked_at"`
}

type WebhookCategory struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

type WebhookEntry struct {
	ID          int64               `json:"id"`
	UserID      int64               `json:"user_id"`
	FeedID      int64               `json:"feed_id"`
	Status      string              `json:"status"`
	Hash        string              `json:"hash"`
	Title       string              `json:"title"`
	URL         string              `json:"url"`
	CommentsURL string              `json:"comments_url"`
	Date        time.Time           `json:"published_at"`
	CreatedAt   time.Time           `json:"created_at"`
	ChangedAt   time.Time           `json:"changed_at"`
	Content     string              `json:"content"`
	Author      string              `json:"author"`
	ShareCode   string              `json:"share_code"`
	Starred     bool                `json:"starred"`
	ReadingTime int                 `json:"reading_time"`
	Enclosures  model.EnclosureList `json:"enclosures"`
	Tags        []string            `json:"tags"`
	Feed        *WebhookFeed        `json:"feed,omitempty"`
}

type WebhookNewEntriesEvent struct {
	EventType  string          `json:"event_type"`
	DeliveryID string          `json:"delivery_id"`
	Feed       *WebhookFeed    `json:"feed"`
	Entries    []*WebhookEntry `json:"entries"`
}

func (e *WebhookNewEntriesEvent) setDeliveryID(deliveryID string) {
	e.DeliveryID = deliveryID
}

type WebhookSaveEntryEvent struct {
	EventType  string        `json:"event_type"`
	DeliveryID string        `json:"delivery_id"`
	Entry      *WebhookEntry `json:"entry"`
}

func (e *WebhookSaveEntryEvent) setDeliveryID(deliveryID string) {
	e.DeliveryID = deliveryID
}
