package tigerbeetle

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	tbtypes "github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tb"
)

// fakeClient implements tb.Client with in-memory storage. It does NOT
// simulate TB's balance accounting — tests inspect recorded transfers
// directly. Anything that needs real balance math is in integration_test.go
// behind the build tag.
type fakeClient struct {
	accounts             []tbtypes.Account
	transfers            []tbtypes.Transfer
	createTransfersCalls int
	createAccountsErr    error
	createTransfersErr   error
	transferResults   map[int]tbtypes.CreateTransferResult
	accountResults    map[int]tbtypes.CreateAccountResult
	lookupAccount     map[tbtypes.Uint128]tbtypes.Account
	queryByCode       map[uint16][]tbtypes.Transfer
	closed            bool

	// health-test hooks
	lookupAccountsErr   error
	lookupAccountsBlock chan struct{} // non-nil: LookupAccounts blocks until closed
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		transferResults: map[int]tbtypes.CreateTransferResult{},
		accountResults:  map[int]tbtypes.CreateAccountResult{},
		lookupAccount:   map[tbtypes.Uint128]tbtypes.Account{},
		queryByCode:     map[uint16][]tbtypes.Transfer{},
	}
}

func (f *fakeClient) CreateAccounts(accounts []tbtypes.Account) ([]tbtypes.AccountEventResult, error) {
	if f.createAccountsErr != nil {
		return nil, f.createAccountsErr
	}
	results := make([]tbtypes.AccountEventResult, 0, len(accounts))
	for i, a := range accounts {
		f.accounts = append(f.accounts, a)
		res := tbtypes.AccountOK
		if override, ok := f.accountResults[len(f.accounts)-1]; ok {
			res = override
		}
		results = append(results, tbtypes.AccountEventResult{Index: uint32(i), Result: res})
	}
	return results, nil
}

func (f *fakeClient) CreateTransfers(transfers []tbtypes.Transfer) ([]tbtypes.TransferEventResult, error) {
	f.createTransfersCalls++
	if f.createTransfersErr != nil {
		return nil, f.createTransfersErr
	}
	results := make([]tbtypes.TransferEventResult, 0, len(transfers))
	for i, t := range transfers {
		f.transfers = append(f.transfers, t)
		res := tbtypes.TransferOK
		if override, ok := f.transferResults[len(f.transfers)-1]; ok {
			res = override
		}
		results = append(results, tbtypes.TransferEventResult{Index: uint32(i), Result: res})
	}
	return results, nil
}

func (f *fakeClient) LookupAccounts(ids []tbtypes.Uint128) ([]tbtypes.Account, error) {
	if f.lookupAccountsBlock != nil {
		<-f.lookupAccountsBlock
	}
	if f.lookupAccountsErr != nil {
		return nil, f.lookupAccountsErr
	}
	var out []tbtypes.Account
	for _, id := range ids {
		if a, ok := f.lookupAccount[id]; ok {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeClient) LookupTransfers([]tbtypes.Uint128) ([]tbtypes.Transfer, error) {
	return nil, nil
}

func (f *fakeClient) GetAccountTransfers(filter tbtypes.AccountFilter) ([]tbtypes.Transfer, error) {
	var out []tbtypes.Transfer
	for _, t := range f.transfers {
		if t.DebitAccountID == filter.AccountID || t.CreditAccountID == filter.AccountID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeClient) GetAccountBalances(tbtypes.AccountFilter) ([]tbtypes.AccountBalance, error) {
	return nil, nil
}

func (f *fakeClient) QueryAccounts(tbtypes.QueryFilter) ([]tbtypes.Account, error) {
	return nil, nil
}

func (f *fakeClient) QueryTransfers(filter tbtypes.QueryFilter) ([]tbtypes.Transfer, error) {
	var out []tbtypes.Transfer
	for _, t := range f.transfers {
		if filter.UserData128 != (tbtypes.Uint128{}) && t.UserData128 != filter.UserData128 {
			continue
		}
		if filter.Code != 0 && t.Code != filter.Code {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeClient) Nop() error  { return nil }
func (f *fakeClient) Close()       { f.closed = true }

// silentLogger returns a logger that discards output, keeping test
// stderr clean of expected error paths.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// advUUID builds a deterministic advertiser UUID for tests.
func advUUID(name string) string { return idgen.Derive("account", "adv-"+name) }
func pubUUID(name string) string { return idgen.Derive("account", "pub-"+name) }

func TestLedgerSpendProducesTwoTransfers(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	pub := pubUUID("dailynews")
	id := l.Record(billing.LedgerEntry{
		Type:             billing.EntrySpend,
		TraceID:          "trace-001",
		AdvertiserID:     adv,
		PublisherID:      pub,
		DebitAccount:     "advertiser:" + adv,
		CreditAccount:    "publisher:" + pub,
		Amount:           1.00,
		PublisherRevenue: 0.80,
		PlatformMargin:   0.20,
		Currency:         "USD",
		BidModel:         "cpm",
	})
	if id == 0 {
		t.Fatal("Record returned 0 — implies an internal error path was hit")
	}
	if got, want := len(fc.transfers), 2; got != want {
		t.Fatalf("Spend should produce %d transfers, got %d", want, got)
	}

	rev, mar := fc.transfers[0], fc.transfers[1]
	if rev.Code != tb.CodeSpend {
		t.Errorf("revenue transfer code = %d, want CodeSpend(%d)", rev.Code, tb.CodeSpend)
	}
	if !rev.TransferFlags().Linked {
		t.Error("revenue transfer should have Linked=true (head of pair)")
	}
	if mar.Code != tb.CodeMargin {
		t.Errorf("margin transfer code = %d, want CodeMargin(%d)", mar.Code, tb.CodeMargin)
	}
	if mar.TransferFlags().Linked {
		t.Error("margin transfer should NOT have Linked=true (terminator)")
	}
	if mar.CreditAccountID != tb.HouseAccountID {
		t.Error("margin must credit the house account")
	}
}

// RecordBatch collapses N entries into ONE CreateTransfers request while
// producing the exact same transfers as N per-entry Record calls — the
// throughput lever (N events → ~1 round-trip instead of N).
func TestRecordBatchOneCallSameTransfers(t *testing.T) {
	mk := func(trace, advKey, pubKey string, rev, mar float64) billing.LedgerEntry {
		adv, pub := advUUID(advKey), pubUUID(pubKey)
		return billing.LedgerEntry{
			Type: billing.EntrySpend, TraceID: trace, AdvertiserID: adv, PublisherID: pub,
			DebitAccount: "advertiser:" + adv, CreditAccount: "publisher:" + pub,
			Amount: rev + mar, PublisherRevenue: rev, PlatformMargin: mar, Currency: "USD", BidModel: "cpm",
		}
	}
	entries := []billing.LedgerEntry{
		mk("trace-a", "acme", "dailynews", 0.80, 0.20),
		mk("trace-b", "globex", "dailynews", 4.00, 1.00),
		mk("trace-c", "acme", "sports", 2.40, 0.60),
	}

	batchFC := newFakeClient()
	New(batchFC, silentLogger()).RecordBatch(entries)

	perFC := newFakeClient()
	perLedger := New(perFC, silentLogger())
	for _, e := range entries {
		perLedger.Record(e)
	}

	if batchFC.createTransfersCalls != 1 {
		t.Errorf("RecordBatch made %d CreateTransfers calls, want 1 (batched)", batchFC.createTransfersCalls)
	}
	if perFC.createTransfersCalls != len(entries) {
		t.Errorf("per-entry made %d CreateTransfers calls, want %d", perFC.createTransfersCalls, len(entries))
	}
	if got, want := len(batchFC.transfers), len(entries)*2; got != want {
		t.Errorf("batch produced %d transfers, want %d (2 per CPM spend)", got, want)
	}
	if !reflect.DeepEqual(batchFC.transfers, perFC.transfers) {
		t.Errorf("batched transfers differ from per-entry:\n batch=%+v\n per  =%+v", batchFC.transfers, perFC.transfers)
	}
}

func TestLedgerReservationProducesPendingTransfer(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	l.Record(billing.LedgerEntry{
		Type:          billing.EntryReservation,
		TraceID:       "trace-002",
		AdvertiserID:  adv,
		DebitAccount:  "advertiser:" + adv,
		CreditAccount: "escrow:res-trace-002",
		Amount:        2.50,
		Currency:      "USD",
		BidModel:      "cpc",
		ReservationID: "res-trace-002",
	})

	if got, want := len(fc.transfers), 1; got != want {
		t.Fatalf("Reservation should produce %d transfer, got %d", want, got)
	}
	tr := fc.transfers[0]
	flags := tr.TransferFlags()
	if !flags.Pending {
		t.Error("reservation must have Pending flag")
	}
	if tr.Code != tb.CodeReservation {
		t.Errorf("code = %d, want CodeReservation(%d)", tr.Code, tb.CodeReservation)
	}
	if tr.CreditAccountID != tb.EscrowAccountID {
		t.Error("reservation must credit escrow")
	}
	if tr.Timeout != tb.ReservationTimeoutSeconds {
		t.Errorf("timeout = %d, want %d (24h)", tr.Timeout, tb.ReservationTimeoutSeconds)
	}
	if tb.UnpackBidModel(tr.UserData32) != "cpc" {
		t.Errorf("user_data_32 didn't pack bid model: got %q", tb.UnpackBidModel(tr.UserData32))
	}
}

func TestLedgerSettlementProducesThreeLinkedTransfers(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	pub := pubUUID("dailynews")
	l.Record(billing.LedgerEntry{
		Type:             billing.EntrySettlement,
		TraceID:          "trace-003",
		AdvertiserID:     adv,
		PublisherID:      pub,
		DebitAccount:     "escrow:res-trace-003",
		CreditAccount:    "publisher:" + pub,
		Amount:           2.50,
		PublisherRevenue: 2.00,
		PlatformMargin:   0.50,
		Currency:         "USD",
		BidModel:         "cpc",
		ReservationID:    "res-trace-003",
	})

	if got, want := len(fc.transfers), 3; got != want {
		t.Fatalf("Settlement should produce %d transfers, got %d", want, got)
	}
	post, rev, mar := fc.transfers[0], fc.transfers[1], fc.transfers[2]

	postFlags := post.TransferFlags()
	if !postFlags.PostPendingTransfer {
		t.Error("first settle transfer must be PostPendingTransfer")
	}
	if !postFlags.Linked {
		t.Error("first settle transfer must be Linked (chain head)")
	}
	if post.PendingID != tb.ReservationID("trace-003") {
		t.Error("post-pending must reference the reservation ID")
	}
	// The post-pending transfer MUST inherit the pending reservation's code
	// (0), not force CodeSettlement — real TB rejects a code mismatch with
	// pending_transfer_has_different_code. The "settlement happened" signal
	// lives on the escrow→publisher transfer below (CodeSettlement).
	if post.Code != 0 {
		t.Errorf("post-pending code = %d, want 0 (inherit pending's code); a nonzero mismatch is rejected by TigerBeetle", post.Code)
	}
	if rev.Code != tb.CodeSettlement {
		t.Errorf("revenue transfer code = %d, want CodeSettlement(%d) — the settlement signal", rev.Code, tb.CodeSettlement)
	}

	if !rev.TransferFlags().Linked {
		t.Error("publisher revenue transfer must be Linked (middle of chain)")
	}
	if rev.DebitAccountID != tb.EscrowAccountID || rev.CreditAccountID == tb.HouseAccountID {
		t.Error("publisher revenue transfer must move escrow → publisher")
	}

	if mar.TransferFlags().Linked {
		t.Error("margin transfer must NOT be Linked (chain terminator)")
	}
	if mar.CreditAccountID != tb.HouseAccountID {
		t.Error("margin transfer must credit the house account")
	}
}

func TestLedgerReleaseProducesVoidPendingTransfer(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	l.Record(billing.LedgerEntry{
		Type:         billing.EntryRelease,
		TraceID:      "trace-004",
		AdvertiserID: adv,
	})

	if got, want := len(fc.transfers), 1; got != want {
		t.Fatalf("Release should produce %d transfer, got %d", want, got)
	}
	tr := fc.transfers[0]
	if !tr.TransferFlags().VoidPendingTransfer {
		t.Error("release must have VoidPendingTransfer flag")
	}
	if tr.PendingID != tb.ReservationID("trace-004") {
		t.Error("void must reference the reservation ID")
	}
}

func TestLedgerEnsureAccountIsCached(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	pub := pubUUID("dailynews")
	entry := billing.LedgerEntry{
		Type:             billing.EntrySpend,
		AdvertiserID:     adv,
		PublisherID:      pub,
		DebitAccount:     "advertiser:" + adv,
		CreditAccount:    "publisher:" + pub,
		Amount:           1.00,
		PublisherRevenue: 0.80,
		PlatformMargin:   0.20,
		BidModel:         "cpm",
	}
	l.Record(entry)
	firstAccountCount := len(fc.accounts)

	entry.TraceID = "trace-second"
	l.Record(entry)
	secondAccountCount := len(fc.accounts)

	if firstAccountCount != secondAccountCount {
		t.Errorf("second Record should hit the account cache; account count went %d → %d", firstAccountCount, secondAccountCount)
	}
}

func TestLedgerSummaryAccumulates(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	pub := pubUUID("dailynews")
	for i := 0; i < 3; i++ {
		l.Record(billing.LedgerEntry{
			Type:             billing.EntrySpend,
			AdvertiserID:     adv,
			PublisherID:      pub,
			DebitAccount:     "advertiser:" + adv,
			CreditAccount:    "publisher:" + pub,
			Amount:           1.00,
			PublisherRevenue: 0.80,
			PlatformMargin:   0.20,
			BidModel:         "cpm",
		})
	}
	s := l.Summary()
	if s.TotalEntries != 3 {
		t.Errorf("TotalEntries = %d, want 3", s.TotalEntries)
	}
	if s.TotalSpend != 3.00 {
		t.Errorf("TotalSpend = %v, want 3.00", s.TotalSpend)
	}
	if s.TotalPublisherRevenue < 2.39 || s.TotalPublisherRevenue > 2.41 {
		t.Errorf("TotalPublisherRevenue = %v, want ~2.40", s.TotalPublisherRevenue)
	}
	if s.TotalPlatformMargin < 0.59 || s.TotalPlatformMargin > 0.61 {
		t.Errorf("TotalPlatformMargin = %v, want ~0.60", s.TotalPlatformMargin)
	}
}

func TestLedgerHasSettlement(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	pub := pubUUID("dailynews")
	l.Record(billing.LedgerEntry{
		Type:             billing.EntrySettlement,
		TraceID:          "trace-has",
		AdvertiserID:     adv,
		PublisherID:      pub,
		DebitAccount:     "escrow:res-trace-has",
		CreditAccount:    "publisher:" + pub,
		Amount:           1.00,
		PublisherRevenue: 0.80,
		PlatformMargin:   0.20,
		BidModel:         "cpc",
	})

	if !l.HasSettlement("trace-has") {
		t.Fatal("HasSettlement should return true after a Settlement was recorded")
	}
	if l.HasSettlement("trace-never-settled") {
		t.Fatal("HasSettlement should return false for unrecorded trace")
	}
}

func TestLedgerReservationByTrace(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	l.Record(billing.LedgerEntry{
		Type:          billing.EntryReservation,
		TraceID:       "trace-res",
		AdvertiserID:  adv,
		DebitAccount:  "advertiser:" + adv,
		CreditAccount: "escrow:res-trace-res",
		Amount:        5.00,
		BidModel:      "vcpm",
	})
	got, ok := l.ReservationByTrace("trace-res")
	if !ok {
		t.Fatal("ReservationByTrace should find the recorded reservation")
	}
	if got.Type != billing.EntryReservation {
		t.Errorf("returned entry type = %s, want reservation", got.Type)
	}
	if got.BidModel != "vcpm" {
		t.Errorf("bid model survived TB round-trip as %q, want vcpm", got.BidModel)
	}
	if _, ok := l.ReservationByTrace("trace-nope"); ok {
		t.Fatal("ReservationByTrace should return false for unknown trace")
	}
}

func TestRecordOnTBErrorReturnsZero(t *testing.T) {
	fc := newFakeClient()
	// Force the second transfer in a CPM spend pair to fail. Engine
	// callers discard the return today, but a zero ID is the documented
	// signal.
	fc.transferResults[1] = tbtypes.TransferExistsWithDifferentAmount
	l := New(fc, silentLogger())

	adv := advUUID("acme")
	pub := pubUUID("dailynews")
	id := l.Record(billing.LedgerEntry{
		Type:             billing.EntrySpend,
		TraceID:          "trace-err",
		AdvertiserID:     adv,
		PublisherID:      pub,
		DebitAccount:     "advertiser:" + adv,
		CreditAccount:    "publisher:" + pub,
		Amount:           1.00,
		PublisherRevenue: 0.80,
		PlatformMargin:   0.20,
		BidModel:         "cpm",
	})
	if id != 0 {
		t.Fatalf("Record on TB error should return 0, got %d", id)
	}
}

func TestRecordUnsupportedTypeReturnsZero(t *testing.T) {
	fc := newFakeClient()
	l := New(fc, silentLogger())

	id := l.Record(billing.LedgerEntry{Type: billing.EntryAdjustment, TraceID: "trace-adj"})
	if id != 0 {
		t.Fatalf("Adjustment is not yet supported; expected 0, got %d", id)
	}
}

// TestRecordBatch_ReplayNoStorm verifies a redelivered entry inside a batch (its
// transfers come back Exists/LinkedEventFailed) does NOT trigger the whole-chunk
// per-entry retry — which previously re-submitted the already-committed sibling
// chains and logged them as failures. The batch stays ONE CreateTransfers call and
// every entry, including the idempotent replay, counts as recorded.
func TestRecordBatch_ReplayNoStorm(t *testing.T) {
	mk := func(trace, advKey, pubKey string, rev, mar float64) billing.LedgerEntry {
		adv, pub := advUUID(advKey), pubUUID(pubKey)
		return billing.LedgerEntry{
			Type: billing.EntrySpend, TraceID: trace, AdvertiserID: adv, PublisherID: pub,
			DebitAccount: "advertiser:" + adv, CreditAccount: "publisher:" + pub,
			Amount: rev + mar, PublisherRevenue: rev, PlatformMargin: mar, Currency: "USD", BidModel: "cpm",
		}
	}
	entries := []billing.LedgerEntry{
		mk("trace-a", "acme", "dailynews", 0.80, 0.20),
		mk("trace-b", "globex", "dailynews", 4.00, 1.00),
		mk("trace-c", "acme", "sports", 2.40, 0.60),
	}
	fc := newFakeClient()
	// Entry B (transfers at chunk index 2,3) is a replay: spend Exists, margin cascades.
	fc.transferResults[2] = tbtypes.TransferExists
	fc.transferResults[3] = tbtypes.TransferLinkedEventFailed

	l := New(fc, silentLogger())
	l.RecordBatch(entries)

	if fc.createTransfersCalls != 1 {
		t.Fatalf("CreateTransfers calls = %d, want 1 (a replay must NOT trigger the per-entry retry storm)", fc.createTransfersCalls)
	}
	if got := l.Summary().TotalEntries; got != 3 {
		t.Fatalf("recorded entries = %d, want 3 (the idempotent replay still counts as recorded)", got)
	}
}

// TestRecordBatch_RealFailureIsolated verifies a genuinely-failed chain is dropped
// (logged) while the sibling chains TB committed still count — and the whole chunk
// is NOT re-submitted per-entry.
func TestRecordBatch_RealFailureIsolated(t *testing.T) {
	mk := func(trace, advKey, pubKey string, rev, mar float64) billing.LedgerEntry {
		adv, pub := advUUID(advKey), pubUUID(pubKey)
		return billing.LedgerEntry{
			Type: billing.EntrySpend, TraceID: trace, AdvertiserID: adv, PublisherID: pub,
			DebitAccount: "advertiser:" + adv, CreditAccount: "publisher:" + pub,
			Amount: rev + mar, PublisherRevenue: rev, PlatformMargin: mar, Currency: "USD", BidModel: "cpm",
		}
	}
	entries := []billing.LedgerEntry{
		mk("trace-a", "acme", "dailynews", 0.80, 0.20),
		mk("trace-b", "globex", "dailynews", 4.00, 1.00),
		mk("trace-c", "acme", "sports", 2.40, 0.60),
	}
	fc := newFakeClient()
	// Entry B's spend has a REAL failure (different amount on the same ID); margin cascades.
	fc.transferResults[2] = tbtypes.TransferExistsWithDifferentAmount
	fc.transferResults[3] = tbtypes.TransferLinkedEventFailed

	l := New(fc, silentLogger())
	l.RecordBatch(entries)

	if fc.createTransfersCalls != 1 {
		t.Fatalf("CreateTransfers calls = %d, want 1 (no whole-chunk retry on a real failure)", fc.createTransfersCalls)
	}
	if got := l.Summary().TotalEntries; got != 2 {
		t.Fatalf("recorded entries = %d, want 2 (A and C commit, B fails)", got)
	}
}
