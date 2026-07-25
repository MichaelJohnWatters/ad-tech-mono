package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/billing"
)

// ContractRow is a publisher revenue-share contract as the warm cache stores it.
// The Contract field is pre-decoded from publishers.revshare_config JSONB so
// downstream consumers can call billing.Engine without further parsing.
type ContractRow struct {
	PublisherID string
	Contract    *billing.Contract
}

// ContractLoader reads publisher revenue-share contracts.
//
// Joins publishers + revshare_config; produces one ContractRow per publisher.
// Per-row JSON decoding happens here so the hot billing path stays allocation-free.
type ContractLoader struct {
	Store *Store
}

func (l *ContractLoader) LoadAll(ctx context.Context) ([]ContractRow, error) {
	const q = `
SELECT
    id::text,
    currency,
    revshare_model,
    COALESCE(revshare_config::text, '{}')
FROM publishers
WHERE status = 'active'`

	rows, closeRows, err := l.Store.QueryPlatform(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query contracts: %w", err)
	}
	defer closeRows()

	var out []ContractRow
	for rows.Next() {
		var publisherID, currency, model, cfgJSON string
		if err := rows.Scan(&publisherID, &currency, &model, &cfgJSON); err != nil {
			return nil, fmt.Errorf("scan contract: %w", err)
		}
		out = append(out, ContractRow{
			PublisherID: publisherID,
			Contract:    parseContract(model, currency, cfgJSON),
		})
	}
	return out, rows.Err()
}

func (l *ContractLoader) KeyOf(r ContractRow) string { return r.PublisherID }

// parseContract converts the publishers row + revshare_config JSON into the
// billing engine's Contract type. Schema-permissive: any field absent from the
// JSON falls back to the publisher's defaults (fixed model, fee_pct=20).
func parseContract(model, currency, cfgJSON string) *billing.Contract {
	var raw struct {
		FeePct            float64            `json:"fee_pct"`
		Tiers             []billing.Tier     `json:"tiers"`
		GuaranteedMinCPM  float64            `json:"guaranteed_min_cpm"`
		DealTypeModifiers map[string]float64 `json:"deal_type_modifiers"`
	}
	_ = json.Unmarshal([]byte(cfgJSON), &raw)

	if currency == "" {
		currency = "USD"
	}
	c := &billing.Contract{
		Model:             billing.RevenueModel(model),
		FeePct:            raw.FeePct,
		Tiers:             raw.Tiers,
		GuaranteedMinCPM:  raw.GuaranteedMinCPM,
		DealTypeModifiers: raw.DealTypeModifiers,
		Currency:          currency,
	}
	if c.Model == "" {
		c.Model = billing.ModelFixed
	}
	if c.FeePct == 0 {
		c.FeePct = 20
	}
	return c
}
