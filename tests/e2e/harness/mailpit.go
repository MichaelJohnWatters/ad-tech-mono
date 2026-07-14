//go:build e2e

package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// mailpitURL is the Mailpit REST API (Tilt port-forwards the UI+API to 8025).
const mailpitURL = "http://localhost:8025"

// MailpitMessage is one delivered email as Mailpit's search API reports it.
type MailpitMessage struct {
	ID      string `json:"ID"`
	Subject string `json:"Subject"`
	To      []struct {
		Address string `json:"Address"`
	} `json:"To"`
}

// MailpitSearch returns the messages delivered to a recipient, newest first.
func (h *Harness) MailpitSearch(t *testing.T, to string) []MailpitMessage {
	t.Helper()
	resp, err := http.Get(mailpitURL + "/api/v1/search?query=" + url.QueryEscape("to:"+to))
	if err != nil {
		t.Fatalf("mailpit search: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mailpit search status %d", resp.StatusCode)
	}
	var out struct {
		Messages []MailpitMessage `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("mailpit search decode: %v", err)
	}
	return out.Messages
}

// MailpitBody returns the plain-text body of a delivered message.
func (h *Harness) MailpitBody(t *testing.T, id string) string {
	t.Helper()
	resp, err := http.Get(mailpitURL + "/api/v1/message/" + id)
	if err != nil {
		t.Fatalf("mailpit message %s: %v", id, err)
	}
	defer resp.Body.Close()
	var out struct {
		Text string `json:"Text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("mailpit message decode: %v", err)
	}
	return out.Text
}

// WaitForMail polls Mailpit until a message to the recipient arrives (or the
// timeout elapses) and returns it.
func (h *Harness) WaitForMail(t *testing.T, timeout time.Duration, to string) MailpitMessage {
	t.Helper()
	var got MailpitMessage
	WaitFor(t, timeout, fmt.Sprintf("email delivered to %s", to), func() bool {
		msgs := h.MailpitSearch(t, to)
		if len(msgs) == 0 {
			return false
		}
		got = msgs[0]
		return true
	})
	return got
}
