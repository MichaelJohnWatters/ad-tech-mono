package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

const (
	traceT      = "trace-abc"
	advOwnerID  = "acct-advertiser-A"
	pubID       = "pub-site-X"
	winnerDSPID = "dsp-internal-SECRET"
	// Public audience segments on the win record — internal bidding detail
	// that must never reach the publisher view.
	segA = "sports_fans_SEGSECRET"
	segB = "auto_intenders_SEGSECRET"
)

func seedTraceFixture(t *testing.T) (analytics.Store, billing.Ledger) {
	t.Helper()
	store := analytics.NewMemory()
	now := time.Now()
	if err := store.InsertAuctionWin(context.Background(), &analytics.AuctionWinEvent{
		TraceID: traceT, WinnerDSP: winnerDSPID, CampaignID: "camp1", CreativeID: "crea1",
		PlacementID: "plc1", PublisherID: pubID, AdvertiserID: advOwnerID, ClearingPrice: 5.00,
		BidModel: "cpm", Segments: []string{segA, segB}, Timestamp: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertImpression(context.Background(), &analytics.ImpressionEvent{
		TraceID: traceT, CampaignID: "camp1", CreativeID: "crea1", PlacementID: "plc1",
		PublisherID: pubID, AccountID: advOwnerID, ClearingPriceUSD: 0.005, BidModel: "cpm",
		Timestamp: now.Add(120 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	ledger := billing.NewMemoryLedger()
	ledger.Record(billing.LedgerEntry{
		Type: billing.EntrySpend, TraceID: traceT, CampaignID: "camp1", AdvertiserID: advOwnerID,
		PublisherID: pubID, Amount: 0.005, PublisherRevenue: 0.004, PlatformMargin: 0.001, Currency: "USD",
	})
	return store, ledger
}

func doTrace(t *testing.T, store analytics.Store, ledger billing.Ledger, acctType, acctID, publisherID string) *httptest.ResponseRecorder {
	t.Helper()
	h := traceHandler(store, ledger, logger.New("test"))
	req := httptest.NewRequest(http.MethodGet, routes.ReportingTrace+"?trace_id="+traceT, nil)
	if acctType != "" {
		req.Header.Set(constants.HeaderAccountType, acctType)
	}
	if acctID != "" {
		req.Header.Set(constants.HeaderAccountID, acctID)
	}
	if publisherID != "" {
		// The trusted, gateway-validated publisher scope header.
		req.Header.Set(constants.HeaderPublisherID, publisherID)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// TestTrace_StaffSeesEverything: staff is unscoped and sees the winner DSP +
// full financial split.
func TestTrace_StaffSeesEverything(t *testing.T) {
	store, ledger := seedTraceFixture(t)
	rec := doTrace(t, store, ledger, string(auth.AccountStaff), "", "")
	if rec.Code != 200 {
		t.Fatalf("staff status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, winnerDSPID) {
		t.Errorf("staff should see winner DSP %q; body=%s", winnerDSPID, body)
	}
	if !strings.Contains(body, "margin") {
		t.Errorf("staff should see platform margin; body=%s", body)
	}
	if !strings.Contains(body, "Audience resolved") || !strings.Contains(body, segA) || !strings.Contains(body, segB) {
		t.Errorf("staff should see the Audience resolved step with both segments; body=%s", body)
	}
}

// TestTrace_AdvertiserRedacted: the owning advertiser sees their spend but NOT
// the winner DSP, publisher revenue, or platform margin.
func TestTrace_AdvertiserRedacted(t *testing.T) {
	store, ledger := seedTraceFixture(t)
	rec := doTrace(t, store, ledger, string(auth.AccountAdvertiser), advOwnerID, "")
	if rec.Code != 200 {
		t.Fatalf("advertiser status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, forbidden := range []string{winnerDSPID, "margin", "Publisher rev", "0.001", "0.004"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("advertiser response leaked %q; body=%s", forbidden, body)
		}
	}
	if !strings.Contains(body, "Your spend") {
		t.Errorf("advertiser should see their spend; body=%s", body)
	}
	// The winning advertiser DOES see the audience the request resolved to.
	if !strings.Contains(body, "Audience resolved") || !strings.Contains(body, segA) {
		t.Errorf("advertiser should see the Audience resolved step; body=%s", body)
	}
	// Decodes cleanly + has steps.
	var resp traceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Steps) == 0 {
		t.Error("expected steps")
	}
}

// TestTrace_WrongAdvertiser404: an advertiser that doesn't own the trace gets a
// 404 (indistinguishable from not-found — no cross-tenant leak).
func TestTrace_WrongAdvertiser404(t *testing.T) {
	store, ledger := seedTraceFixture(t)
	rec := doTrace(t, store, ledger, string(auth.AccountAdvertiser), "acct-someone-else", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong advertiser status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), winnerDSPID) {
		t.Error("404 body must not leak trace contents")
	}
}

// TestTrace_PublisherNeedsValidatedID: a publisher session with no (gateway-
// validated) publisher_id is denied rather than served unscoped data.
func TestTrace_PublisherNeedsValidatedID(t *testing.T) {
	store, ledger := seedTraceFixture(t)
	rec := doTrace(t, store, ledger, string(auth.AccountPublisher), "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("publisher w/o publisher_id status = %d, want 403", rec.Code)
	}
	// With their publisher_id it works and shows revenue but NOT advertiser id / margin.
	rec = doTrace(t, store, ledger, string(auth.AccountPublisher), "", pubID)
	if rec.Code != 200 {
		t.Fatalf("publisher w/ publisher_id status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, winnerDSPID) || strings.Contains(body, advOwnerID) || strings.Contains(body, "margin") {
		t.Errorf("publisher response leaked advertiser/winner/margin; body=%s", body)
	}
	// Audience segments are internal bidding detail — a publisher must not
	// learn which segments their visitor is in.
	if strings.Contains(body, "Audience resolved") || strings.Contains(body, segA) || strings.Contains(body, segB) {
		t.Errorf("publisher response leaked audience segments; body=%s", body)
	}
	if !strings.Contains(body, "Your revenue") {
		t.Errorf("publisher should see their revenue; body=%s", body)
	}
}

// TestTrace_UnknownTypeDenied: a missing/unknown account type must not default
// to unscoped.
func TestTrace_UnknownTypeDenied(t *testing.T) {
	store, ledger := seedTraceFixture(t)
	rec := doTrace(t, store, ledger, "", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing account type status = %d, want 403", rec.Code)
	}
}
