package notifications

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// TestTranslate_BudgetDepleted is the representative event→notification mapping:
// a budget-depleted payload becomes a per-account row with the campaign as the
// deep-link ref and a human title/body.
func TestTranslate_BudgetDepleted(t *testing.T) {
	data, _ := json.Marshal(events.BudgetDepletedEvent{
		SchemaVersion: events.CurrentSchemaVersion,
		CampaignID:    "camp-123",
		AccountID:     "acct-abc",
		Budget:        100,
		Spent:         100,
		Timestamp:     time.Now(),
	})

	n, ok := Translate(events.SubjectBudgetDepleted, data)
	if !ok {
		t.Fatal("Translate returned ok=false for a valid budget-depleted event")
	}
	if n.AccountID != "acct-abc" {
		t.Errorf("AccountID = %q, want acct-abc", n.AccountID)
	}
	if n.Kind != KindBudgetDepleted {
		t.Errorf("Kind = %q, want %q", n.Kind, KindBudgetDepleted)
	}
	if n.RefID != "camp-123" {
		t.Errorf("RefID = %q, want camp-123 (campaign deep-link)", n.RefID)
	}
	if n.Title == "" || !strings.Contains(strings.ToLower(n.Title), "budget") {
		t.Errorf("Title should mention budget: %q", n.Title)
	}
}

// TestTranslate_NoAccountIsDropped: a payload with no account owner is not a
// notification (nothing to show a user) — the consumer acks it without writing.
func TestTranslate_NoAccountIsDropped(t *testing.T) {
	data, _ := json.Marshal(events.BalanceDepletedEvent{
		SchemaVersion: events.CurrentSchemaVersion,
		AccountID:     "", // no owner
	})
	if _, ok := Translate(events.SubjectBalanceDepleted, data); ok {
		t.Error("Translate should drop an event with no account_id")
	}
}

// TestTranslate_UnmappedSubject: an unrelated subject yields ok=false.
func TestTranslate_UnmappedSubject(t *testing.T) {
	if _, ok := Translate(events.SubjectImpression, []byte(`{}`)); ok {
		t.Error("Translate should return ok=false for an unmapped subject")
	}
}

// TestTranslate_CampaignState carries old/new state into the body + campaign ref.
func TestTranslate_CampaignState(t *testing.T) {
	data, _ := json.Marshal(events.CampaignStateEvent{
		SchemaVersion: events.CurrentSchemaVersion,
		CampaignID:    "camp-9",
		AccountID:     "acct-9",
		OldState:      "live",
		NewState:      "paused",
	})
	n, ok := Translate(events.SubjectCampaignStateChanged, data)
	if !ok {
		t.Fatal("ok=false for campaign state event")
	}
	if n.Kind != KindCampaignState || n.RefID != "camp-9" {
		t.Errorf("got kind=%q ref=%q", n.Kind, n.RefID)
	}
	if !strings.Contains(n.Body, "paused") || !strings.Contains(n.Body, "live") {
		t.Errorf("body should carry state transition: %q", n.Body)
	}
}
