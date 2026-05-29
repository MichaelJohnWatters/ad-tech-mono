// Package webhooks provides webhook registration and delivery.
// Consumes events from NATS and delivers HTTP POST to registered URLs.
//
// Usage:
//
//	dispatcher := webhooks.NewDispatcher(logger)
//	dispatcher.Register(webhooks.Webhook{URL: "https://example.com/hook", Events: []string{"impression"}})
//	dispatcher.Dispatch(ctx, "impression", payload)
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Webhook is a registered webhook endpoint.
type Webhook struct {
	ID        string
	AccountID string
	URL       string
	Secret    string   // HMAC signing secret
	Events    []string // event types to subscribe to
	Status    string   // active, paused, failed
	CreatedAt time.Time
}

// Delivery tracks a webhook delivery attempt.
type Delivery struct {
	WebhookID  string
	EventType  string
	URL        string
	StatusCode int
	Success    bool
	Error      string
	Timestamp  time.Time
	RetryCount int
}

// Dispatcher manages webhook registrations and delivers events.
type Dispatcher struct {
	mu        sync.RWMutex
	webhooks  map[string]*Webhook
	deliveries []Delivery
	client    *http.Client
	log       *slog.Logger
}

// NewDispatcher creates a webhook dispatcher.
func NewDispatcher(log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		webhooks: make(map[string]*Webhook),
		client:   &http.Client{Timeout: 10 * time.Second},
		log:      log,
	}
}

// Register adds a webhook.
func (d *Dispatcher) Register(wh Webhook) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if wh.ID == "" {
		wh.ID = fmt.Sprintf("wh-%d", len(d.webhooks)+1)
	}
	if wh.CreatedAt.IsZero() {
		wh.CreatedAt = time.Now()
	}
	wh.Status = "active"
	d.webhooks[wh.ID] = &wh
}

// Remove deletes a webhook.
func (d *Dispatcher) Remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.webhooks, id)
}

// List returns all webhooks for an account.
func (d *Dispatcher) List(accountID string) []Webhook {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var result []Webhook
	for _, wh := range d.webhooks {
		if accountID == "" || wh.AccountID == accountID {
			result = append(result, *wh)
		}
	}
	return result
}

// Dispatch sends an event to all matching webhooks.
func (d *Dispatcher) Dispatch(ctx context.Context, eventType string, payload interface{}) {
	d.mu.RLock()
	var targets []*Webhook
	for _, wh := range d.webhooks {
		if wh.Status != "active" {
			continue
		}
		for _, et := range wh.Events {
			if et == eventType || et == "*" {
				targets = append(targets, wh)
				break
			}
		}
	}
	d.mu.RUnlock()

	for _, wh := range targets {
		go d.deliver(ctx, wh, eventType, payload)
	}
}

func (d *Dispatcher) deliver(ctx context.Context, wh *Webhook, eventType string, payload interface{}) {
	body, err := json.Marshal(map[string]interface{}{
		"event":     eventType,
		"timestamp": time.Now().UTC(),
		"data":      payload,
	})
	if err != nil {
		d.recordDelivery(wh.ID, eventType, wh.URL, 0, false, err.Error())
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", wh.URL, bytes.NewReader(body))
	if err != nil {
		d.recordDelivery(wh.ID, eventType, wh.URL, 0, false, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Webhook-Event", eventType)

	// Sign with HMAC if secret is set
	if wh.Secret != "" {
		sig := signPayload(body, wh.Secret)
		req.Header.Set("X-Webhook-Signature", sig)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		d.recordDelivery(wh.ID, eventType, wh.URL, 0, false, err.Error())
		d.log.Warn("webhook delivery failed", "webhook", wh.ID, "url", wh.URL, "error", err)
		return
	}
	resp.Body.Close()

	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	d.recordDelivery(wh.ID, eventType, wh.URL, resp.StatusCode, success, "")

	if !success {
		d.log.Warn("webhook delivery rejected", "webhook", wh.ID, "status", resp.StatusCode)
	}
}

func (d *Dispatcher) recordDelivery(webhookID, eventType, url string, status int, success bool, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deliveries = append(d.deliveries, Delivery{
		WebhookID:  webhookID,
		EventType:  eventType,
		URL:        url,
		StatusCode: status,
		Success:    success,
		Error:      errMsg,
		Timestamp:  time.Now(),
	})
}

// DeliveryHistory returns recent deliveries.
func (d *Dispatcher) DeliveryHistory(webhookID string, limit int) []Delivery {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var result []Delivery
	for i := len(d.deliveries) - 1; i >= 0 && len(result) < limit; i-- {
		del := d.deliveries[i]
		if webhookID == "" || del.WebhookID == webhookID {
			result = append(result, del)
		}
	}
	return result
}

func signPayload(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
