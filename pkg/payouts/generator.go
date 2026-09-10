// Package payouts generates publisher payouts — the money-OUT mirror of
// pkg/invoicing (money-IN). For each publisher, it sums the period's gross
// revenue (winning clearing prices on that publisher's inventory, from the
// analytics store), applies the publisher's revenue-share contract to get the
// net owed, gates on the publisher's configured minimum-payout threshold, and
// writes one idempotent `payouts` row per (publisher, period).
//
// Earnings source: the same formula the reporting "net_revenue" metric uses
// (gross × contract), so a payout matches what the publisher's earnings console
// shows. No ledger mutation — a payout is a SETTLEMENT RECORD (an attestation
// that publisher X earned $N in period P), consumed by the read-side
// cmd/gateway/payouts.go and, eventually, a real disbursement.
package payouts

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/postgres"
)

// GrossReader returns period gross revenue (sum of winning clearing_price_usd)
// per publisher_id over [start, end). Backed by the analytics store (ClickHouse)
// in production; a fake in tests.
type GrossReader interface {
	GrossByPublisher(ctx context.Context, start, end time.Time) (map[string]float64, error)
}

// Generator writes publisher payouts. Reads (contracts, payout methods,
// publisher→account map) go through the platform_read hatch — the runner sums
// EVERY publisher, so a tenant-scoped read would blank the cross-publisher
// enumeration. Each payout WRITE is scoped to the owning account's GUC so the
// payouts RLS policy (migration 017) admits it.
type Generator struct {
	store *postgres.Store
	gross GrossReader
}

// New builds a Generator over a Postgres store + a gross-revenue reader.
func New(store *postgres.Store, gross GrossReader) *Generator {
	return &Generator{store: store, gross: gross}
}

// Result is the outcome of a generation pass.
type Result struct {
	Written   int // payout rows actually inserted/updated
	Unchanged int // publishers whose existing payout was already paid/processing (upsert no-op'd)
	HeldLow   int // publishers skipped: net below their minimum-payout threshold
	ZeroRev   int // publishers with no gross revenue in the period
}

// publisher is the per-publisher facts the run needs, joined once up front.
type publisher struct {
	id       string
	account  string
	contract *billing.Contract
	minCents int64  // payout_methods.minimum_payout_cents (0 = no minimum)
	currency string // payout currency (payout_method's, else the contract's)
}

// GenerateForAllPublishers writes a payout for every active publisher whose net
// earnings over [start, end) clear their minimum threshold. Idempotent: a re-run
// UPSERTs the same (publisher, period) row while it is still 'pending'.
func (g *Generator) GenerateForAllPublishers(ctx context.Context, start, end time.Time) (Result, error) {
	pubs, err := g.loadPublishers(ctx)
	if err != nil {
		return Result{}, err
	}
	gross, err := g.gross.GrossByPublisher(ctx, start, end)
	if err != nil {
		return Result{}, fmt.Errorf("gross by publisher: %w", err)
	}

	var res Result
	for _, p := range pubs {
		grossUSD := gross[p.id]
		if grossUSD <= 0 {
			res.ZeroRev++
			continue
		}
		amount, fee, ok := ComputePayout(grossUSD, p.contract, p.minCents)
		if !ok {
			res.HeldLow++
			continue
		}
		wrote, err := g.write(ctx, p, amount, fee, start, end)
		if err != nil {
			return res, fmt.Errorf("write payout for publisher %s: %w", p.id, err)
		}
		if wrote {
			res.Written++
		} else {
			res.Unchanged++ // an already paid/processing payout — upsert no-op'd
		}
	}
	return res, nil
}

// GenerateForPublisher writes (or refreshes) a single publisher's payout for the
// period — used by the account-closeout final-payout path. Returns ("", nil,
// held=true) when the net is below the minimum threshold or there is no revenue.
func (g *Generator) GenerateForPublisher(ctx context.Context, publisherID string, start, end time.Time) (written bool, err error) {
	pubs, err := g.loadPublishers(ctx)
	if err != nil {
		return false, err
	}
	var p *publisher
	for i := range pubs {
		if pubs[i].id == publisherID {
			p = &pubs[i]
			break
		}
	}
	if p == nil {
		return false, fmt.Errorf("publisher %s not found / inactive", publisherID)
	}
	gross, err := g.gross.GrossByPublisher(ctx, start, end)
	if err != nil {
		return false, fmt.Errorf("gross by publisher: %w", err)
	}
	grossUSD := gross[p.id]
	if grossUSD <= 0 {
		return false, nil
	}
	amount, fee, ok := ComputePayout(grossUSD, p.contract, p.minCents)
	if !ok {
		return false, nil
	}
	return g.write(ctx, *p, amount, fee, start, end)
}

// ComputePayout is the pure payout math: net = gross × (1 − fee%), applying the
// publisher's contract (fixed/tiered/guaranteed/deal-type), rounded to cents.
// Returns (amount, platformFee, write) where write=false means the net is below
// the publisher's minimum-payout threshold and no row should be written (the
// earnings are held, not carried forward — a documented MVP limitation).
func ComputePayout(gross float64, c *billing.Contract, minCents int64) (amount, platformFee float64, write bool) {
	rev := c.CalculateRevenue(gross, "")
	// Work in integer cents so the split reconciles EXACTLY: derive the fee as
	// grossCents − amountCents rather than rounding the margin independently (which
	// drifts a cent, creating/destroying money — and float subtraction like
	// 1.0−0.8 isn't exact). amount + platform_fee == gross-at-cents by construction.
	grossCents := int64(math.Round(gross * 100))
	amountCents := int64(math.Round(rev.PublisherRevenue * 100))
	feeCents := grossCents - amountCents // negative when a guaranteed-min contract subsidizes above gross
	amount = float64(amountCents) / 100
	platformFee = float64(feeCents) / 100
	if amountCents <= 0 {
		return 0, 0, false
	}
	if amountCents < minCents {
		return amount, platformFee, false
	}
	return amount, platformFee, true
}

// loadPublishers joins active publishers to their contract + active payout
// method under the platform_read hatch (cross-publisher enumeration).
func (g *Generator) loadPublishers(ctx context.Context) ([]publisher, error) {
	// Contracts (publisher_id → *billing.Contract) via the shared loader.
	loader := &postgres.ContractLoader{Store: g.store}
	contracts, err := loader.LoadAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("load contracts: %w", err)
	}
	byID := make(map[string]*billing.Contract, len(contracts))
	for _, c := range contracts {
		byID[c.PublisherID] = c.Contract
	}

	// publisher → account + currency, LEFT JOIN its active payout method for the
	// minimum threshold + payout currency. platform_read hatch: cross-publisher.
	const q = `
SELECT p.id::text, p.account_id::text, COALESCE(p.currency,'USD'),
       COALESCE(pm.minimum_payout_cents, 0),
       COALESCE(NULLIF(pm.currency,''), p.currency, 'USD')
FROM publishers p
LEFT JOIN payout_methods pm ON pm.account_id = p.account_id AND pm.status = 'active'
WHERE p.status = 'active'`
	rows, closeRows, err := g.store.QueryPlatform(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("load publishers: %w", err)
	}
	defer closeRows()

	var out []publisher
	for rows.Next() {
		var p publisher
		var pubCurrency string
		if err := rows.Scan(&p.id, &p.account, &pubCurrency, &p.minCents, &p.currency); err != nil {
			return nil, fmt.Errorf("scan publisher: %w", err)
		}
		p.contract = byID[p.id]
		if p.contract == nil {
			// No contract row (shouldn't happen for active publishers) — skip
			// rather than pay out at an unknown rate.
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// write UPSERTs one payout row, scoped to the owning account's tenant GUC so the
// payouts RLS policy admits it. ON CONFLICT refreshes amount/platform_fee ONLY
// while the payout is still 'pending' — an already 'processing'/'paid' payout is
// never rewritten by a re-run. Returns wrote=false (RowsAffected 0) when the
// status guard blocked the upsert, so the caller doesn't count a no-op as a fresh
// payout.
func (g *Generator) write(ctx context.Context, p publisher, amount, fee float64, start, end time.Time) (wrote bool, err error) {
	tx, err := g.store.Primary().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT set_config('app.current_account_id', $1, true)`, p.account); err != nil {
		return false, fmt.Errorf("set tenant GUC: %w", err)
	}
	const up = `
INSERT INTO payouts (publisher_id, account_id, amount, currency, platform_fee, status, period_start, period_end)
VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'pending', $6, $7)
ON CONFLICT (publisher_id, period_start, period_end) DO UPDATE
   SET amount = EXCLUDED.amount, platform_fee = EXCLUDED.platform_fee, currency = EXCLUDED.currency
   WHERE payouts.status = 'pending'`
	res, err := tx.ExecContext(ctx, up, p.id, p.account, amount, p.currency, fee,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}
