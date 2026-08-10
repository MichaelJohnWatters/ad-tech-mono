// marketplace.go — data-MARKETPLACE surcharge settlement (PLAN Phase 10, slice 3).
//
// When an INTERNAL buyer wins an impression on a campaign that targets a segment
// they PURCHASED (a marketplace grant), the listing's CPM surcharge settles per
// DELIVERED impression:
//
//	debit  advertiser:{buyer}:balance            surcharge         (buyer pays)
//	credit advertiser:{seller}:balance           surcharge − margin (seller earns net)
//	credit platform:marketplace_surcharge_margin margin
//
// plus one marketplace_surcharge_earnings row per (trace, segment) — the PK is
// the exactly-once claim (INSERT … ON CONFLICT DO NOTHING RETURNING gates the
// ledger writes, so a redelivered impression settles at most once).
//
// Unlike the data-fee flow there is no parked SSP event: the trigger is the
// buyer's own impression + an active grant + the winning campaign targeting the
// granted segment. All computed at impression time in reporting (async, off the
// bid path — no DSP/hot-path change).
//
// ATTRIBUTION NOTE (documented approximation): this settles when the winning
// campaign TARGETS a granted segment — the buyer bought that data and is
// serving on a campaign that uses it. Strict "the user matched THIS segment (vs
// another targeted one)" attribution needs the DSP to stamp the matched segment
// on the bid response; deferred. For the common case (a campaign buys a segment
// to target it) the two coincide. The bid-time balance gate also does not yet
// account for the surcharge (the buyer's balance can go slightly negative on
// surcharges under a tight balance) — a documented follow-up.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/analytics"
)

type marketplaceAccrual struct {
	db *sql.DB
	// marginPctFn reads reporting.marketplace_surcharge_margin_pct live per accrual.
	marginPctFn func() float64
	log         *slog.Logger
}

func newMarketplaceAccrual(db *sql.DB, marginPctFn func() float64, log *slog.Logger) *marketplaceAccrual {
	if db == nil {
		return nil
	}
	return &marketplaceAccrual{db: db, marginPctFn: marginPctFn, log: log}
}

// AccrueOnImpression settles any marketplace surcharge for one impression.
// Called on EVERY impression; the cheap probe (does the buyer have ANY active
// grant?) makes it a fast miss for the vast majority. Failures log at ERROR and
// return — the impression is already recorded + billed; surcharge accrual must
// never fail the impression.
func (a *marketplaceAccrual) AccrueOnImpression(ctx context.Context, e *analytics.ImpressionEvent) {
	if a == nil || e == nil || e.TraceID == "" || e.AccountID == "" || e.CampaignID == "" {
		return
	}
	// The whole settlement runs in ONE platform-hatch tx: the grant+targeting
	// JOIN doubles as the probe AND the cross-tenant read/write (debit buyer,
	// credit seller, book platform margin). marketplace_grants has RLS (slice 2),
	// so the read MUST be under the hatch — a bare read on the app-role pool sees
	// zero rows and silently no-ops. The tenant_isolation policies are USING-only,
	// so platform_read admits both the read and the writes (security #77). Perf
	// note: this is a tx per impression (no cheap non-tx probe, unlike data-fee's
	// no-RLS pending table) — cache the small "advertisers with grants" set in
	// reporting to skip the tx for the grant-less majority if volume warrants.
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		a.log.Error("marketplace surcharge begin failed", "trace_id", e.TraceID, "error", err)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.platform_read', 'on', true)`); err != nil {
		a.log.Error("marketplace surcharge platform-read set failed", "trace_id", e.TraceID, "error", err)
		return
	}

	// The granted segments this winning campaign TARGETS (the intersection of the
	// buyer's active grants with the campaign's include_segments). include_segments
	// stores segment ids as text; grant segment_id is a uuid → cast to match.
	rows, err := tx.QueryContext(ctx, `
SELECT g.segment_id::text, g.seller_account_id::text, g.cpm_surcharge_micros
FROM marketplace_grants g
JOIN targeting_rules t ON t.line_item_id = $2::uuid
WHERE g.buyer_account_id = $1::uuid AND g.status = 'active'
  AND (g.expires_at IS NULL OR g.expires_at > now())
  AND g.segment_id::text = ANY(t.include_segments)`, e.AccountID, e.CampaignID)
	if err != nil {
		a.log.Error("marketplace surcharge grant match failed", "trace_id", e.TraceID, "error", err)
		return
	}
	type match struct {
		segmentID, sellerID string
		cpmMicros           int64
	}
	var matches []match
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.segmentID, &m.sellerID, &m.cpmMicros); err != nil {
			rows.Close()
			a.log.Error("marketplace surcharge scan failed", "trace_id", e.TraceID, "error", err)
			return
		}
		matches = append(matches, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		a.log.Error("marketplace surcharge rows err", "trace_id", e.TraceID, "error", err)
		return
	}
	if len(matches) == 0 {
		return // the buyer has grants, but none for a segment THIS campaign targets
	}

	marginPct := a.marginPctFn()
	if marginPct < 0 {
		marginPct = 0
	}
	if marginPct > 100 {
		marginPct = 100
	}
	settled := 0
	for _, m := range matches {
		surchargeMicros := m.cpmMicros / 1000 // CPM → per-impression micros
		if surchargeMicros <= 0 {
			continue
		}
		marginMicros := int64(float64(surchargeMicros) * marginPct / 100)
		sellerNetMicros := surchargeMicros - marginMicros

		// The earnings row IS the exactly-once claim: only if this INSERT wins the
		// (trace, segment) PK do the ledger + balance writes run. A redelivered
		// impression conflicts → no row → no double settlement.
		var claimed int
		err := tx.QueryRowContext(ctx, `
INSERT INTO marketplace_surcharge_earnings
  (trace_id, segment_id, buyer_account_id, seller_account_id, publisher_id, placement_id,
   surcharge_micros, seller_net_micros, margin_micros)
VALUES ($1, $2, $3::uuid, $4::uuid, $5, $6, $7, $8, $9)
ON CONFLICT (trace_id, segment_id) DO NOTHING
RETURNING 1`,
			e.TraceID, m.segmentID, e.AccountID, m.sellerID, e.PublisherID, e.PlacementID,
			surchargeMicros, sellerNetMicros, marginMicros).Scan(&claimed)
		if err == sql.ErrNoRows {
			continue // already settled for this (trace, segment) — redelivery
		}
		if err != nil {
			a.log.Error("marketplace surcharge earnings insert failed", "trace_id", e.TraceID, "segment", m.segmentID, "error", err)
			return
		}

		surchargeUSD := float64(surchargeMicros) / 1e6
		netUSD := float64(sellerNetMicros) / 1e6
		marginUSD := float64(marginMicros) / 1e6
		ref := e.TraceID + "/" + m.segmentID
		if _, err := tx.ExecContext(ctx, `
INSERT INTO ledger_entries (account_code, entry_type, amount, currency, reference_type, reference_id)
VALUES ('advertiser:' || $1 || ':balance', 'debit',  $2, 'USD', 'marketplace_surcharge', $5),
       ('advertiser:' || $3 || ':balance', 'credit', $4, 'USD', 'marketplace_surcharge', $5),
       ('platform:marketplace_surcharge_margin', 'credit', $6, 'USD', 'marketplace_surcharge', $5)`,
			e.AccountID, surchargeUSD, m.sellerID, netUSD, ref, marginUSD); err != nil {
			a.log.Error("marketplace surcharge ledger insert failed", "trace_id", e.TraceID, "segment", m.segmentID, "error", err)
			return
		}
		// Real money: buyer's prepay balance shrinks by the surcharge, seller's
		// grows by the net.
		if _, err := tx.ExecContext(ctx, `
INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
VALUES ($1::uuid, -($2::numeric), 'USD', now())
ON CONFLICT (account_id) DO UPDATE
  SET balance = advertiser_balances.balance - $2::numeric, updated_at = now()`,
			e.AccountID, surchargeUSD); err != nil {
			a.log.Error("marketplace surcharge buyer debit failed", "trace_id", e.TraceID, "buyer", e.AccountID, "error", err)
			return
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO advertiser_balances (account_id, balance, currency, updated_at)
VALUES ($1::uuid, $2, 'USD', now())
ON CONFLICT (account_id) DO UPDATE
  SET balance = advertiser_balances.balance + $2, updated_at = now()`,
			m.sellerID, netUSD); err != nil {
			a.log.Error("marketplace surcharge seller credit failed", "trace_id", e.TraceID, "seller", m.sellerID, "error", err)
			return
		}
		settled++
	}
	if err := tx.Commit(); err != nil {
		a.log.Error("marketplace surcharge commit failed", "trace_id", e.TraceID, "error", err)
		return
	}
	if settled > 0 {
		a.log.Info("marketplace surcharge settled", "trace_id", e.TraceID, "buyer", e.AccountID,
			"segments", settled, "margin_pct", fmt.Sprintf("%.1f", marginPct))
	}
}
