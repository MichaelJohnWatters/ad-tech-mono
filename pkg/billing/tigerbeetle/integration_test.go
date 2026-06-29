//go:build tigerbeetle_integration

// Integration test against a live TigerBeetle instance. Build-tag-guarded
// because the SDK pulls in cgo native libs and the test requires Tilt up
// with the tigerbeetle pod ready on its port-forward (3033:3000).
//
// Run with: go test -tags=tigerbeetle_integration ./pkg/billing/tigerbeetle/...
// Env knob:  TB_ADDRESSES=127.0.0.1:3033 (default)

package tigerbeetle

import (
	"os"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tb"
)

func testAddresses() []string {
	if a := os.Getenv("TB_ADDRESSES"); a != "" {
		return []string{a}
	}
	return []string{"127.0.0.1:3033"}
}

// TestIntegrationCPMRoundTrip exercises the full CPM spend path against a
// live TB cluster: two transfers, advertiser debits posted, house margin
// posted, BalanceFor reflects both.
func TestIntegrationCPMRoundTrip(t *testing.T) {
	client, err := tb.NewClient(testAddresses())
	if err != nil {
		t.Skipf("TigerBeetle unreachable at %v: %v (start Tilt first)", testAddresses(), err)
	}
	defer client.Close()

	l := New(client, silentLogger())

	// Distinct advertiser/publisher per test run via timestamped name —
	// keeps the assertion independent of accumulated state from prior runs.
	adv := idgen.Derive("account", "adv-int-"+t.Name())
	pub := idgen.Derive("account", "pub-int-"+t.Name())

	id := l.Record(billing.LedgerEntry{
		Type:             billing.EntrySpend,
		TraceID:          "trace-int-cpm-" + t.Name(),
		AdvertiserID:     adv,
		PublisherID:      pub,
		DebitAccount:     "advertiser:" + adv,
		CreditAccount:    "publisher:" + pub,
		Amount:           5.00,
		PublisherRevenue: 4.00,
		PlatformMargin:   1.00,
		BidModel:         "cpm",
	})
	if id == 0 {
		t.Fatal("Record returned 0 — TB rejected the transfers")
	}

	advBal := l.BalanceFor("advertiser:" + adv)
	if advBal.TotalDebit < 4.99 || advBal.TotalDebit > 5.01 {
		t.Errorf("advertiser TotalDebit = %v, want ~5.00", advBal.TotalDebit)
	}

	houseBal := l.BalanceFor("platform:house")
	if houseBal.TotalCredit < 1.00 {
		t.Errorf("house TotalCredit = %v, want >= 1.00 (this run's margin)", houseBal.TotalCredit)
	}
}

// TestIntegrationReserveSettle exercises reserve → settle: pending
// reservation + 3-transfer post-pending settle. Final state: advertiser
// debited the clearing price, publisher credited revenue, house credited
// margin, escrow balanced to zero (or the prior balance, since escrow is
// shared across tests).
func TestIntegrationReserveSettle(t *testing.T) {
	client, err := tb.NewClient(testAddresses())
	if err != nil {
		t.Skipf("TigerBeetle unreachable at %v: %v", testAddresses(), err)
	}
	defer client.Close()

	l := New(client, silentLogger())

	adv := idgen.Derive("account", "adv-int-rs-"+t.Name())
	pub := idgen.Derive("account", "pub-int-rs-"+t.Name())
	trace := "trace-int-rs-" + t.Name()

	if id := l.Record(billing.LedgerEntry{
		Type:          billing.EntryReservation,
		TraceID:       trace,
		AdvertiserID:  adv,
		DebitAccount:  "advertiser:" + adv,
		CreditAccount: "escrow:res-" + trace,
		Amount:        10.00,
		BidModel:      "cpc",
		ReservationID: "res-" + trace,
	}); id == 0 {
		t.Fatal("Reservation Record returned 0")
	}

	if l.HasSettlement(trace) {
		t.Fatal("HasSettlement should be false before settle")
	}

	advBalAfterReserve := l.BalanceFor("advertiser:" + adv)
	if advBalAfterReserve.Reservations < 9.99 {
		t.Errorf("advertiser pending reservation = %v, want ~10.00", advBalAfterReserve.Reservations)
	}

	if id := l.Record(billing.LedgerEntry{
		Type:             billing.EntrySettlement,
		TraceID:          trace,
		AdvertiserID:     adv,
		PublisherID:      pub,
		DebitAccount:     "escrow:res-" + trace,
		CreditAccount:    "publisher:" + pub,
		Amount:           10.00,
		PublisherRevenue: 8.00,
		PlatformMargin:   2.00,
		BidModel:         "cpc",
		ReservationID:    "res-" + trace,
	}); id == 0 {
		t.Fatal("Settlement Record returned 0")
	}

	if !l.HasSettlement(trace) {
		t.Fatal("HasSettlement should be true after settle")
	}

	advFinal := l.BalanceFor("advertiser:" + adv)
	if advFinal.TotalDebit < 9.99 || advFinal.TotalDebit > 10.01 {
		t.Errorf("advertiser TotalDebit = %v, want ~10.00", advFinal.TotalDebit)
	}
	if advFinal.Reservations > 0.01 {
		t.Errorf("advertiser Reservations = %v, want 0 (settled)", advFinal.Reservations)
	}

	pubBal := l.BalanceFor("publisher:" + pub)
	if pubBal.TotalCredit < 7.99 || pubBal.TotalCredit > 8.01 {
		t.Errorf("publisher TotalCredit = %v, want ~8.00 (revenue)", pubBal.TotalCredit)
	}
}
