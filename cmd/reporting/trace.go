package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

// The trace inspector endpoints reconstruct a single request's flow (and list
// recent impressions to inspect) for the tenant portals. Scoping + redaction are
// enforced HERE, server-side, per the caller's X-Account-Type:
//
//   staff/admin — unscoped; full detail (winner DSP, spend, publisher rev, margin)
//   advertiser  — scoped to their account_id; sees their spend, NOT publisher
//                 revenue / platform margin / other bidders
//   publisher   — scoped to their (gateway-validated) publisher_id; sees their
//                 revenue, NOT advertiser identity / platform margin
//
// The gateway injects X-Account-ID / X-Account-Type after auth and validates the
// publisher_id, so these handlers trust those headers.

// traceStep / tracePanel mirror the shape static/trace-render.js draws
// (lowercase JSON keys). The server builds the redacted list; the portal just
// hands the response to renderTrace().
type traceStep struct {
	Time    string `json:"time"`
	Service string `json:"service"`
	Cls     string `json:"cls"`
	Msg     string `json:"msg"`
	Detail  string `json:"detail"`
}

type tracePanel struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Label string `json:"label"`
	Cls   string `json:"cls"`
}

type traceResponse struct {
	TraceID string       `json:"traceId"`
	Note    string       `json:"note"`
	Steps   []traceStep  `json:"steps"`
	Panels  []tracePanel `json:"panels"`
}

// scopeFromRequest resolves the tenant scope from the gateway-injected headers.
// Returns (scope, unscoped, ok). ok=false means the caller type isn't allowed
// (unknown/missing) — deny rather than default to unscoped.
func scopeFromRequest(r *http.Request) (analytics.TraceScope, bool, bool) {
	acctType := r.Header.Get(constants.HeaderAccountType)
	acctID := r.Header.Get(constants.HeaderAccountID)
	switch acctType {
	case string(auth.AccountStaff), string(auth.AccountAdmin):
		return analytics.TraceScope{}, true, true
	case string(auth.AccountAdvertiser), string(auth.AccountAgency):
		if acctID == "" {
			return analytics.TraceScope{}, false, false
		}
		return analytics.TraceScope{AccountID: acctID}, false, true
	case string(auth.AccountPublisher):
		// Publisher scope comes ONLY from the gateway-validated X-Publisher-ID
		// header — never a client query param — so a publisher can't read another
		// publisher's data. Until the gateway sets it (after validating the id
		// against the session), publishers are denied here.
		pub := r.Header.Get(constants.HeaderPublisherID)
		if pub == "" {
			return analytics.TraceScope{}, false, false
		}
		return analytics.TraceScope{PublisherID: pub}, false, true
	default:
		return analytics.TraceScope{}, false, false
	}
}

// traceHandler serves GET /v1/reporting/trace?trace_id=… — the scoped, redacted
// per-request flow timeline.
func traceHandler(store analytics.Store, ledger billing.Ledger, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		traceID := r.URL.Query().Get("trace_id")
		if traceID == "" {
			http.Error(w, `{"error":"trace_id required"}`, http.StatusBadRequest)
			return
		}
		acctType := r.Header.Get(constants.HeaderAccountType)
		scope, _, ok := scopeFromRequest(r)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		tr, isTR := store.(analytics.TraceReader)
		if !isTR {
			http.Error(w, `{"error":"trace inspection not supported on this backend"}`, http.StatusNotImplemented)
			return
		}
		events, err := tr.EventsByTrace(r.Context(), traceID, scope)
		if err != nil {
			log.Error("trace lookup failed", "trace_id", traceID, "error", err)
			http.Error(w, `{"error":"lookup failed"}`, http.StatusInternalServerError)
			return
		}
		if len(events) == 0 {
			// Not found OR the caller doesn't own it — indistinguishable on purpose
			// (don't leak trace existence across tenants).
			http.Error(w, `{"error":"trace not found"}`, http.StatusNotFound)
			return
		}

		// Financial figures come from the ledger (best-effort — a TB backend may
		// not retain them). Advertiser spend also falls back to the impression's
		// clearing_price_usd, which is always present.
		entries := ledger.EntriesForTrace(traceID)
		resp := traceResponse{
			TraceID: traceID,
			Note:    traceNote(acctType, len(events)),
			Steps:   buildTraceSteps(events, acctType),
			Panels:  buildTracePanels(events, entries, acctType),
		}
		json.NewEncoder(w).Encode(resp)
	}
}

// recentImpressionsHandler serves GET /v1/reporting/recent-impressions?limit=…
// — the scoped list a portal hangs "View trace" off of.
func recentImpressionsHandler(store analytics.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		scope, _, ok := scopeFromRequest(r)
		if !ok {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		tr, isTR := store.(analytics.TraceReader)
		if !isTR {
			http.Error(w, `{"error":"not supported"}`, http.StatusNotImplemented)
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		rows, err := tr.RecentImpressions(r.Context(), scope, limit)
		if err != nil {
			log.Error("recent impressions failed", "error", err)
			http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"impressions": rows})
	}
}

func traceNote(acctType string, n int) string {
	switch acctType {
	case string(auth.AccountAdvertiser), string(auth.AccountAgency):
		return "Your campaign's journey through the platform."
	case string(auth.AccountPublisher):
		return "This impression's journey through your inventory."
	default:
		return fmt.Sprintf("%d events reconstructed from the analytics store (staff · unscoped).", n)
	}
}

func isStaff(acctType string) bool {
	return acctType == string(auth.AccountStaff) || acctType == string(auth.AccountAdmin)
}
func isAdvertiser(acctType string) bool {
	return acctType == string(auth.AccountAdvertiser) || acctType == string(auth.AccountAgency)
}

// buildTraceSteps turns the raw events into a redacted timeline per audience.
func buildTraceSteps(events []analytics.TraceEvent, acctType string) []traceStep {
	if len(events) == 0 {
		return nil
	}
	base := events[0].Timestamp
	steps := make([]traceStep, 0, len(events))
	for _, e := range events {
		off := e.Timestamp.Sub(base).Milliseconds()
		tlabel := "0ms"
		if off > 0 {
			tlabel = "+" + strconv.FormatInt(off, 10) + "ms"
		}
		svc, msg, detail := "tracker", "", ""
		switch e.Kind {
		case "auction_win":
			svc = "exchange"
			price := fmt.Sprintf("clearing $%.2f CPM", e.ClearingPriceUSD)
			if e.DealID != "" {
				price += " · deal " + shortID(e.DealID)
			}
			switch {
			case isStaff(acctType):
				msg = "Auction won by " + orDash(e.WinnerDSP)
				detail = price + " · advertiser " + shortID(e.AdvertiserID) + " · campaign " + shortID(e.CampaignID)
			case isAdvertiser(acctType):
				msg = "You won the auction"
				detail = price
			default: // publisher
				msg = "Auction won — your inventory sold"
				detail = price
			}
		case "impression":
			msg = "Impression recorded"
			detail = "creative " + shortID(e.CreativeID)
			if e.BidModel != "" {
				detail += " · " + e.BidModel
			}
		case "view":
			if e.EventType == "viewable" {
				msg = "Viewable impression (IAB)"
			} else {
				msg = "Impression not viewable"
			}
		case "click":
			msg = "Click"
		case "conversion":
			msg = "Conversion"
			detail = e.EventType
		case "media":
			msg = "Video/audio: " + e.EventType
		}
		steps = append(steps, traceStep{Time: tlabel, Service: svc, Cls: "active", Msg: msg, Detail: detail})
	}
	return steps
}

// buildTracePanels builds the redacted summary cards. Advertiser sees their
// spend; publisher sees their revenue; staff sees the full split.
func buildTracePanels(events []analytics.TraceEvent, entries []billing.LedgerEntry, acctType string) []tracePanel {
	var spend, pubRev, margin float64
	var winner, campaign string
	for _, e := range events {
		if e.Kind == "impression" && e.ClearingPriceUSD > 0 {
			spend = e.ClearingPriceUSD
		}
		if e.Kind == "auction_win" {
			if winner == "" {
				winner = e.WinnerDSP
			}
			if campaign == "" {
				campaign = e.CampaignID
			}
		}
	}
	for _, le := range entries {
		pubRev += le.PublisherRevenue
		margin += le.PlatformMargin
	}
	switch {
	case isStaff(acctType):
		return []tracePanel{
			{Title: "Advertiser spend", Value: fmt.Sprintf("$%.4f", spend), Label: "this impression", Cls: "green"},
			{Title: "Winner", Value: orDash(winner), Label: "DSP", Cls: "blue"},
			{Title: "Publisher rev", Value: fmt.Sprintf("$%.4f", pubRev), Label: "rev share", Cls: "yellow"},
			{Title: "Platform margin", Value: fmt.Sprintf("$%.4f", margin), Label: "take", Cls: "purple"},
		}
	case isAdvertiser(acctType):
		return []tracePanel{
			{Title: "Your spend", Value: fmt.Sprintf("$%.4f", spend), Label: "this impression", Cls: "green"},
			{Title: "Campaign", Value: shortID(campaign), Label: "campaign id", Cls: "blue"},
		}
	default: // publisher
		return []tracePanel{
			{Title: "Your revenue", Value: fmt.Sprintf("$%.4f", pubRev), Label: "rev share", Cls: "green"},
		}
	}
}

// shortID trims a UUID to its first segment for compact display.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	if id == "" {
		return "—"
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
