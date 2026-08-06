package optimise

import (
	"math"
)

// LineItemPerformance holds metrics for a line item within an IO.
type LineItemPerformance struct {
	ID          string
	Impressions int64
	Clicks      int64
	Conversions int64
	Spend       float64
	CTR         float64 // computed
	CPA         float64 // computed
	ROAS        float64 // return on ad spend
}

// BudgetReallocation is a suggested shift of budget between line items.
type BudgetReallocation struct {
	IOId          string
	Reallocations []Reallocation
	Reason        string
}

// Reallocation is a budget change for a single line item.
type Reallocation struct {
	LineItemID      string
	CurrentBudget   float64
	SuggestedBudget float64
	ChangePct       float64 // +20 = increase 20%, -30 = decrease 30%
	Reason          string
}

// ReallocationConfig controls the auto-optimiser.
type ReallocationConfig struct {
	MaxChangePct      float64 // max budget change per cycle (default 20%)
	MinImpressions    int64   // min impressions before considering reallocation
	PerformanceMetric string  // "ctr", "cpa", "roas"
}

// DefaultReallocationConfig returns sensible defaults.
func DefaultReallocationConfig() ReallocationConfig {
	return ReallocationConfig{
		MaxChangePct:      20,
		MinImpressions:    100,
		PerformanceMetric: "ctr",
	}
}

// RebalanceBudgets suggests budget reallocation across line items within an IO.
// Shifts budget from underperformers to top performers.
func RebalanceBudgets(ioID string, items []LineItemPerformance, totalBudget float64, cfg ReallocationConfig) *BudgetReallocation {
	if len(items) < 2 {
		return nil // nothing to rebalance
	}

	// Compute metrics
	for i := range items {
		if items[i].Impressions > 0 {
			items[i].CTR = float64(items[i].Clicks) / float64(items[i].Impressions) * 100
		}
		if items[i].Clicks > 0 {
			items[i].CPA = items[i].Spend / float64(items[i].Conversions+1) // +1 to avoid div/0
		}
	}

	// Score each item
	scores := make(map[string]float64)
	var totalScore float64
	for _, item := range items {
		if item.Impressions < cfg.MinImpressions {
			scores[item.ID] = 1.0 // neutral score for low-data items
		} else {
			switch cfg.PerformanceMetric {
			case "cpa":
				if item.CPA > 0 {
					scores[item.ID] = 1.0 / item.CPA // lower CPA = higher score
				} else {
					scores[item.ID] = 0.5
				}
			case "roas":
				scores[item.ID] = math.Max(item.ROAS, 0.1)
			default: // ctr
				scores[item.ID] = math.Max(item.CTR, 0.01)
			}
		}
		totalScore += scores[item.ID]
	}

	if totalScore == 0 {
		return nil
	}

	// Calculate ideal budget split proportional to performance
	currentPerItem := totalBudget / float64(len(items))
	var reallocations []Reallocation

	for _, item := range items {
		idealPct := scores[item.ID] / totalScore
		idealBudget := totalBudget * idealPct

		// Cap change at MaxChangePct
		changePct := ((idealBudget - currentPerItem) / currentPerItem) * 100
		if changePct > cfg.MaxChangePct {
			changePct = cfg.MaxChangePct
		}
		if changePct < -cfg.MaxChangePct {
			changePct = -cfg.MaxChangePct
		}

		suggestedBudget := currentPerItem * (1 + changePct/100)

		reason := "maintain"
		if changePct > 5 {
			reason = "increase - outperforming"
		} else if changePct < -5 {
			reason = "decrease - underperforming"
		}

		reallocations = append(reallocations, Reallocation{
			LineItemID:      item.ID,
			CurrentBudget:   currentPerItem,
			SuggestedBudget: suggestedBudget,
			ChangePct:       changePct,
			Reason:          reason,
		})
	}

	return &BudgetReallocation{
		IOId:          ioID,
		Reallocations: reallocations,
		Reason:        "performance-based rebalance using " + cfg.PerformanceMetric,
	}
}
