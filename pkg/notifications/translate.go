package notifications

import (
	"encoding/json"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
)

// Translate turns one raw NATS event payload (identified by its subject) into
// the notification to persist. It returns ok=false for a payload that carries
// no account owner (nothing to show a user) or an unmapped subject — the
// consumer acks those without writing a row. Keeping the human title/body here
// (not in the consumer) makes the mapping unit-testable without NATS.
func Translate(subject string, data []byte) (Notification, bool) {
	switch subject {
	case events.SubjectBudgetDepleted:
		var e events.BudgetDepletedEvent
		if json.Unmarshal(data, &e) != nil || e.AccountID == "" {
			return Notification{}, false
		}
		return Notification{
			AccountID: e.AccountID,
			Kind:      KindBudgetDepleted,
			Title:     "Campaign paused — budget depleted",
			Body:      fmt.Sprintf("A campaign hit its budget cap and stopped bidding (spent %s of %s).", money(e.Spent), money(e.Budget)),
			RefID:     e.CampaignID,
		}, true

	case events.SubjectBalanceDepleted:
		var e events.BalanceDepletedEvent
		if json.Unmarshal(data, &e) != nil || e.AccountID == "" {
			return Notification{}, false
		}
		return Notification{
			AccountID: e.AccountID,
			Kind:      KindBalanceDepleted,
			Title:     "Bidding stopped — balance depleted",
			Body:      "Your prepay balance is exhausted. All campaigns are paused until you top up.",
		}, true

	case events.SubjectCampaignStateChanged:
		var e events.CampaignStateEvent
		if json.Unmarshal(data, &e) != nil || e.AccountID == "" {
			return Notification{}, false
		}
		body := fmt.Sprintf("Campaign state changed from %s to %s.", e.OldState, e.NewState)
		if e.Reason != "" {
			body = fmt.Sprintf("%s (%s)", body, e.Reason)
		}
		return Notification{
			AccountID: e.AccountID,
			Kind:      KindCampaignState,
			Title:     fmt.Sprintf("Campaign %s", e.NewState),
			Body:      body,
			RefID:     e.CampaignID,
		}, true

	case events.SubjectReportCompleted:
		var e events.ReportCompletedEvent
		if json.Unmarshal(data, &e) != nil || e.AccountID == "" {
			return Notification{}, false
		}
		return Notification{
			AccountID: e.AccountID,
			Kind:      KindReportReady,
			Title:     "Report ready",
			Body:      fmt.Sprintf("Your report %q (%d rows) is ready to download.", e.Name, e.RowCount),
			RefID:     e.JobID,
		}, true
	}
	return Notification{}, false
}

// money formats a USD amount for a human-readable notification body.
func money(v float64) string { return fmt.Sprintf("$%.2f", v) }
