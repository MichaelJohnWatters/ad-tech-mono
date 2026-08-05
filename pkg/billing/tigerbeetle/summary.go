// Cluster-global, durable Summary() via stats bucket accounts.
//
// The old Summary() was a per-process accumulator bumped on Record(). With
// reporting at N replicas, NATS delivers each billing event to ONE pod, so
// every pod held a different partial total and /v1/billing/summary answered
// differently depending on which pod served the read (and reset to zero on
// restart). TB has no GROUP BY, so instead each recorded entry ALSO posts
// tiny "stat" transfers on a separate StatsLedger: one bucket account per
// LedgerSummary field, credits_posted = lifetime total in micros (the
// entries bucket counts entries, amount=1). Summary() is then a single
// LookupAccounts — same answer from every pod, durable across restarts.
//
// Stat transfer IDs derive from (bucket, entry-type:trace), so a redelivered
// event replays as TB Exists and can't double-count. Stats are submitted in
// a SEPARATE CreateTransfers request after the money transfers commit: a
// stats failure never blocks money movement (it logs at ERROR and the
// in-process accumulator still catches up as the fallback).
package tigerbeetle

import (
	tbtypes "github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tb"
)

// Summary bucket names — one per billing.LedgerSummary field.
const (
	bucketSpend            = "spend"
	bucketReserved         = "reserved"
	bucketSettled          = "settled"
	bucketReleased         = "released"
	bucketRefunded         = "refunded"
	bucketPublisherRevenue = "publisher_revenue"
	bucketPlatformMargin   = "platform_margin"
	bucketEntries          = "entries"
)

// statsBuckets maps each bucket to its pre-derived TB account ID (derivation
// is a UUIDv5 hash — do it once, not per entry on the hot path).
var statsBuckets = map[string]tbtypes.Uint128{
	bucketSpend:            tb.StatsBucketAccountID(bucketSpend),
	bucketReserved:         tb.StatsBucketAccountID(bucketReserved),
	bucketSettled:          tb.StatsBucketAccountID(bucketSettled),
	bucketReleased:         tb.StatsBucketAccountID(bucketReleased),
	bucketRefunded:         tb.StatsBucketAccountID(bucketRefunded),
	bucketPublisherRevenue: tb.StatsBucketAccountID(bucketPublisherRevenue),
	bucketPlatformMargin:   tb.StatsBucketAccountID(bucketPlatformMargin),
	bucketEntries:          tb.StatsBucketAccountID(bucketEntries),
}

// statTransfers mirrors bumpSummary: the same buckets a memory summary
// would bump for this entry, as StatsLedger transfers. Zero amounts are
// skipped (recording a 0 changes nothing and wastes a transfer).
func statTransfers(e billing.LedgerEntry) []tbtypes.Transfer {
	entryKey := string(e.Type) + ":" + e.TraceID
	traceUD := tb.TraceUserData(e.TraceID)

	var out []tbtypes.Transfer
	add := func(bucket string, micros uint64) {
		if micros == 0 {
			return
		}
		out = append(out, tbtypes.Transfer{
			ID:              tb.StatTransferID(bucket, entryKey),
			DebitAccountID:  tb.StatsSourceAccountID,
			CreditAccountID: statsBuckets[bucket],
			Amount:          tb.MicrosToAmount(micros),
			UserData128:     traceUD,
			Ledger:          tb.StatsLedger,
			Code:            tb.CodeStat,
		})
	}

	switch e.Type {
	case billing.EntrySpend:
		add(bucketSpend, tb.USDToMicros(e.Amount))
		add(bucketPublisherRevenue, tb.USDToMicros(e.PublisherRevenue))
		add(bucketPlatformMargin, tb.USDToMicros(e.PlatformMargin))
	case billing.EntryReservation:
		add(bucketReserved, tb.USDToMicros(e.Amount))
	case billing.EntrySettlement:
		add(bucketSettled, tb.USDToMicros(e.Amount))
		add(bucketPublisherRevenue, tb.USDToMicros(e.PublisherRevenue))
		add(bucketPlatformMargin, tb.USDToMicros(e.PlatformMargin))
	case billing.EntryRelease:
		add(bucketReleased, tb.USDToMicros(e.Amount))
	case billing.EntryRefund:
		add(bucketRefunded, tb.USDToMicros(e.Amount))
	}
	add(bucketEntries, 1)
	return out
}

// recordStats posts the stat transfers for successfully-recorded entries in
// one request. Failures are logged (house rule: failures are ERROR logs) and
// otherwise swallowed — the money already committed, and Summary() falls
// back to the in-process accumulator when buckets are unreadable.
func (l *Ledger) recordStats(entries []billing.LedgerEntry) {
	if len(entries) == 0 {
		return
	}
	if err := l.ensureStatsAccounts(); err != nil {
		l.log.Error("tigerbeetle stats accounts unavailable, summary buckets skipped",
			"entries", len(entries), "error", err)
		return
	}
	var transfers []tbtypes.Transfer
	for _, e := range entries {
		transfers = append(transfers, statTransfers(e)...)
	}
	for start := 0; start < len(transfers); {
		end := min(start+l.perReqCap(), len(transfers))
		if err := l.createTransfers(transfers[start:end]); err != nil {
			// Oversized for the server (e.g. --development wire limit):
			// learn the cap and retry the SAME window at the new size.
			if isBatchSizeExceeded(err) {
				l.shrinkPerReqCap(end - start)
				continue
			}
			l.log.Error("tigerbeetle summary stats write failed (money unaffected)",
				"entries", len(entries), "error", err)
			return
		}
		start = end
	}
}

// ensureStatsAccounts lazily creates the stats source + bucket accounts.
// Same idempotent CreateAccounts path as ensureAccount, cached per process.
func (l *Ledger) ensureStatsAccounts() error {
	if err := l.ensureAccount(tb.StatsSourceAccountID, tb.AccountCodeStats, tb.StatsLedger); err != nil {
		return err
	}
	for _, id := range statsBuckets {
		if err := l.ensureAccount(id, tb.AccountCodeStats, tb.StatsLedger); err != nil {
			return err
		}
	}
	return nil
}

// summaryFromBuckets reads every bucket account in one LookupAccounts and
// rebuilds the LedgerSummary from credits_posted. Buckets that don't exist
// yet (nothing recorded since the cluster was created) read as zero.
func (l *Ledger) summaryFromBuckets() (billing.LedgerSummary, error) {
	ids := make([]tbtypes.Uint128, 0, len(statsBuckets))
	order := []string{
		bucketSpend, bucketReserved, bucketSettled, bucketReleased,
		bucketRefunded, bucketPublisherRevenue, bucketPlatformMargin, bucketEntries,
	}
	for _, b := range order {
		ids = append(ids, statsBuckets[b])
	}
	accs, err := l.client.LookupAccounts(ids)
	l.noteTransport(err)
	if err != nil {
		return billing.LedgerSummary{}, err
	}
	byID := make(map[tbtypes.Uint128]tbtypes.Account, len(accs))
	for _, a := range accs {
		byID[a.ID] = a
	}
	total := func(bucket string) uint64 {
		a, ok := byID[statsBuckets[bucket]]
		if !ok {
			return 0
		}
		micros, ok := tb.AmountToMicros(a.CreditsPosted)
		if !ok {
			l.log.Error("tigerbeetle summary bucket exceeds uint64", "bucket", bucket)
		}
		return micros
	}
	return billing.LedgerSummary{
		TotalEntries:          int(total(bucketEntries)),
		TotalSpend:            tb.MicrosToUSD(total(bucketSpend)),
		TotalReserved:         tb.MicrosToUSD(total(bucketReserved)),
		TotalSettled:          tb.MicrosToUSD(total(bucketSettled)),
		TotalReleased:         tb.MicrosToUSD(total(bucketReleased)),
		TotalRefunded:         tb.MicrosToUSD(total(bucketRefunded)),
		TotalPublisherRevenue: tb.MicrosToUSD(total(bucketPublisherRevenue)),
		TotalPlatformMargin:   tb.MicrosToUSD(total(bucketPlatformMargin)),
	}, nil
}
