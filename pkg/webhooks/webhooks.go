// Package webhooks delivers account-scoped platform events to customer-
// registered HTTP endpoints. The Dispatcher is the testable core: given an
// Event (type + account + raw payload), it looks up the account's active
// subscriptions for that event type, POSTs an HMAC-signed JSON envelope to
// each, retries transient failures with backoff, and records every attempt.
//
// The cmd/webhooks binary wires this to NATS (business-event subjects) and
// Postgres (the `webhooks` + `webhook_deliveries` tables).
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Header names on every delivery. Receivers recompute the signature over the
// raw request body using their subscription secret and compare in constant time.
const (
	HeaderSignature = "X-Adtech-Signature" // "sha256=<hex>"
	HeaderEvent     = "X-Adtech-Event"     // event type, e.g. "budget.depleted"
	HeaderTimestamp = "X-Adtech-Timestamp" // RFC3339 send time
	HeaderDelivery  = "X-Adtech-Delivery"  // webhook subscription id (for receiver logs)
)

// Subscription is one registered endpoint for an account.
type Subscription struct {
	ID     string
	URL    string
	Secret string // HMAC-SHA256 signing secret
}

// Delivery is the record of one POST attempt, persisted for the deliveries log.
type Delivery struct {
	WebhookID      string
	EventType      string
	Payload        []byte
	ResponseStatus int
	ResponseBody   string
	Attempt        int
	Success        bool
}

// Store is the persistence seam: which webhooks fire for an event, and where
// delivery outcomes are written.
type Store interface {
	// ActiveForEvent returns active subscriptions for accountID whose event
	// list contains eventType.
	ActiveForEvent(ctx context.Context, accountID, eventType string) ([]Subscription, error)
	// RecordDelivery persists one attempt outcome (best-effort; a failure to
	// record must not abort delivery).
	RecordDelivery(ctx context.Context, d Delivery) error
}

// Event is an account-scoped platform event to fan out to webhooks. Data is the
// already-marshalled JSON of the originating event (e.g. a BudgetDepletedEvent).
type Event struct {
	Type      string
	AccountID string
	Data      json.RawMessage
}

// envelope is the JSON body actually POSTed. Wrapping the raw event data gives
// receivers a stable, self-describing shape regardless of the inner event.
type envelope struct {
	Event     string          `json:"event"`
	AccountID string          `json:"account_id"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

// Dispatcher fans an Event out to an account's subscriptions.
type Dispatcher struct {
	Store Store
	HTTP  *http.Client
	// MaxAttempts caps tries per subscription (default 3). Backoff returns the
	// pause before the retry after attempt n (default: 1s, 2s, 4s…). Now
	// defaults to time.Now.
	MaxAttempts int
	Backoff     func(attempt int) time.Duration
	Now         func() time.Time
	Log         *slog.Logger

	// sleep is overridable in tests to avoid real backoff waits.
	sleep func(context.Context, time.Duration)
}

func (d *Dispatcher) maxAttempts() int {
	if d.MaxAttempts > 0 {
		return d.MaxAttempts
	}
	return 3
}

func (d *Dispatcher) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Dispatcher) backoff(attempt int) time.Duration {
	if d.Backoff != nil {
		return d.Backoff(attempt)
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s, ...
}

func (d *Dispatcher) httpClient() *http.Client {
	if d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (d *Dispatcher) doSleep(ctx context.Context, dur time.Duration) {
	if d.sleep != nil {
		d.sleep(ctx, dur)
		return
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Dispatch delivers ev to every active subscription for its account. It returns
// an error only on a lookup failure; per-subscription delivery failures are
// recorded and logged, not returned (one bad endpoint must not block others).
func (d *Dispatcher) Dispatch(ctx context.Context, ev Event) error {
	if ev.AccountID == "" || ev.Type == "" {
		return fmt.Errorf("webhooks: event missing type or account_id")
	}
	subs, err := d.Store.ActiveForEvent(ctx, ev.AccountID, ev.Type)
	if err != nil {
		return fmt.Errorf("webhooks: lookup subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}

	body, err := json.Marshal(envelope{
		Event:     ev.Type,
		AccountID: ev.AccountID,
		Timestamp: d.now().UTC().Format(time.RFC3339),
		Data:      ev.Data,
	})
	if err != nil {
		return fmt.Errorf("webhooks: marshal envelope: %w", err)
	}

	for _, sub := range subs {
		d.deliver(ctx, sub, ev.Type, body)
	}
	return nil
}

// deliver POSTs body to one subscription, retrying transient failures up to
// MaxAttempts. Every attempt is recorded. A 2xx is success; anything else (or a
// transport error) is retried until the cap.
func (d *Dispatcher) deliver(ctx context.Context, sub Subscription, eventType string, body []byte) {
	sig := Sign(sub.Secret, body)
	max := d.maxAttempts()

	for attempt := 1; attempt <= max; attempt++ {
		status, respBody, err := d.post(ctx, sub, eventType, body, sig)
		success := err == nil && status >= 200 && status < 300

		rec := Delivery{
			WebhookID:      sub.ID,
			EventType:      eventType,
			Payload:        body,
			ResponseStatus: status,
			ResponseBody:   truncate(respBody, 2048),
			Attempt:        attempt,
			Success:        success,
		}
		if rerr := d.Store.RecordDelivery(ctx, rec); rerr != nil && d.Log != nil {
			d.Log.Warn("webhook delivery not recorded", "webhook_id", sub.ID, "error", rerr)
		}

		if success {
			if d.Log != nil {
				d.Log.Info("webhook delivered", "webhook_id", sub.ID, "event", eventType, "attempt", attempt, "status", status)
			}
			return
		}
		if d.Log != nil {
			d.Log.Warn("webhook delivery failed", "webhook_id", sub.ID, "event", eventType, "attempt", attempt, "status", status, "error", err)
		}
		if attempt < max {
			d.doSleep(ctx, d.backoff(attempt))
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (d *Dispatcher) post(ctx context.Context, sub Subscription, eventType string, body []byte, sig string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSignature, sig)
	req.Header.Set(HeaderEvent, eventType)
	req.Header.Set(HeaderTimestamp, d.now().UTC().Format(time.RFC3339))
	req.Header.Set(HeaderDelivery, sub.ID)

	resp, err := d.httpClient().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(respBody), nil
}

// Sign returns the "sha256=<hex>" HMAC of body under secret — the value placed
// in X-Adtech-Signature. Receivers recompute and compare.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
