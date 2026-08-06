package optimise

import (
	"fmt"
	"time"
)

// Recommendation is an actionable suggestion for a campaign.
type Recommendation struct {
	ID          string
	CampaignID  string
	Type        string // bid_adjustment, targeting, creative, budget, pacing
	Severity    string // info, warning, critical
	Title       string
	Description string
	Action      string // what to do
	Impact      string // expected improvement
	Status      string // pending, applied, dismissed
	CreatedAt   time.Time
}

// CampaignMetrics holds the data used to generate recommendations.
type CampaignMetrics struct {
	CampaignID    string
	Impressions   int64
	Clicks        int64
	Conversions   int64
	Spend         float64
	Budget        float64
	DaysRemaining int
	WinRate       float64
	AvgCPM        float64
	CTR           float64
	ConvRate      float64
	PacingRatio   float64 // actual/expected spend
}

// GenerateRecommendations analyses campaign metrics and produces actionable suggestions.
func GenerateRecommendations(metrics CampaignMetrics) []Recommendation {
	var recs []Recommendation
	now := time.Now()
	id := 0
	nextID := func() string {
		id++
		return fmt.Sprintf("rec-%s-%d", metrics.CampaignID, id)
	}

	// 1. Overpaying detection
	if metrics.WinRate > 0.85 {
		recs = append(recs, Recommendation{
			ID: nextID(), CampaignID: metrics.CampaignID,
			Type: "bid_adjustment", Severity: "warning",
			Title:       "Bid too high - winning 85%+ of auctions",
			Description: fmt.Sprintf("Win rate is %.0f%%. You're likely overpaying. Competitors are bidding lower.", metrics.WinRate*100),
			Action:      "Lower base bid by 15-20% to find optimal price point",
			Impact:      "Save 10-20% on spend with minimal reach loss",
			Status:      "pending", CreatedAt: now,
		})
	}

	// 2. Underbidding detection
	if metrics.WinRate < 0.15 && metrics.Impressions > 100 {
		recs = append(recs, Recommendation{
			ID: nextID(), CampaignID: metrics.CampaignID,
			Type: "bid_adjustment", Severity: "critical",
			Title:       "Bid too low - winning less than 15% of auctions",
			Description: fmt.Sprintf("Win rate is %.0f%%. Most bids are losing. Budget is underutilised.", metrics.WinRate*100),
			Action:      "Increase base bid by 20-30% or broaden targeting",
			Impact:      "2-3x more impressions for the same budget",
			Status:      "pending", CreatedAt: now,
		})
	}

	// 3. Pacing issues
	if metrics.PacingRatio < 0.5 && metrics.DaysRemaining > 3 {
		recs = append(recs, Recommendation{
			ID: nextID(), CampaignID: metrics.CampaignID,
			Type: "pacing", Severity: "warning",
			Title:       "Underpacing - spending too slowly",
			Description: fmt.Sprintf("Pacing at %.0f%% of target. Budget won't be fully spent.", metrics.PacingRatio*100),
			Action:      "Switch to front-loaded or ASAP pacing, or broaden targeting",
			Impact:      "Fully utilise campaign budget",
			Status:      "pending", CreatedAt: now,
		})
	}
	if metrics.PacingRatio > 1.5 {
		recs = append(recs, Recommendation{
			ID: nextID(), CampaignID: metrics.CampaignID,
			Type: "pacing", Severity: "critical",
			Title:       "Overpacing - spending too fast",
			Description: fmt.Sprintf("Pacing at %.0f%% of target. Budget will run out early.", metrics.PacingRatio*100),
			Action:      "Switch to even pacing or reduce bid",
			Impact:      "Extend campaign reach over full flight",
			Status:      "pending", CreatedAt: now,
		})
	}

	// 4. Low CTR
	if metrics.CTR < 0.05 && metrics.Impressions > 1000 {
		recs = append(recs, Recommendation{
			ID: nextID(), CampaignID: metrics.CampaignID,
			Type: "creative", Severity: "warning",
			Title:       "Low click-through rate",
			Description: fmt.Sprintf("CTR is %.2f%%. Industry average is 0.1-0.3%%.", metrics.CTR),
			Action:      "Test new creative variants, review ad copy and CTA",
			Impact:      "2-5x improvement in engagement",
			Status:      "pending", CreatedAt: now,
		})
	}

	// 5. No conversions with clicks
	if metrics.Clicks > 50 && metrics.Conversions == 0 {
		recs = append(recs, Recommendation{
			ID: nextID(), CampaignID: metrics.CampaignID,
			Type: "creative", Severity: "critical",
			Title:       "Clicks but no conversions",
			Description: fmt.Sprintf("%d clicks with 0 conversions. Check if conversion tracking is working.", metrics.Clicks),
			Action:      "Verify conversion pixel is installed and firing correctly",
			Impact:      "May reveal tracking issue hiding real performance",
			Status:      "pending", CreatedAt: now,
		})
	}

	// 6. Budget nearly depleted
	if metrics.Budget > 0 {
		spentPct := metrics.Spend / metrics.Budget * 100
		if spentPct > 90 && metrics.DaysRemaining > 2 {
			recs = append(recs, Recommendation{
				ID: nextID(), CampaignID: metrics.CampaignID,
				Type: "budget", Severity: "info",
				Title:       "Budget nearly depleted",
				Description: fmt.Sprintf("%.0f%% of budget spent with %d days remaining.", spentPct, metrics.DaysRemaining),
				Action:      "Consider adding budget or reducing bid to stretch remaining funds",
				Impact:      "Maintain campaign presence through end of flight",
				Status:      "pending", CreatedAt: now,
			})
		}
	}

	return recs
}
