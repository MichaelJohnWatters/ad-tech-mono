package arbitration

import (
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/publisheradserver"
)

type stubPacing struct{ behind bool }

func (s stubPacing) ShouldServe(_ publisheradserver.PublisherLineItem, _ time.Time) bool {
	return s.behind
}

func active(id, publisher, tier string) publisheradserver.PublisherLineItem {
	return publisheradserver.PublisherLineItem{
		ID:           id,
		PublisherID:  publisher,
		PriorityTier: tier,
		Status:       publisheradserver.StatusActive,
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	req := Request{PublisherID: "pub-1", PlacementID: "pl-1", Now: now}

	cases := []struct {
		name       string
		items      []publisheradserver.PublisherLineItem
		pacing     PacingDecider
		wantType   DecisionType
		wantLineID string
		wantReason string
	}{
		{
			name:       "no line items → programmatic",
			items:      nil,
			pacing:     stubPacing{behind: true},
			wantType:   DecisionProgrammatic,
			wantReason: "no-direct-line-items",
		},
		{
			name: "sponsorship wins over everything",
			items: []publisheradserver.PublisherLineItem{
				active("li-house", "pub-1", publisheradserver.TierHouse),
				active("li-guar", "pub-1", publisheradserver.TierGuaranteed),
				active("li-spon", "pub-1", publisheradserver.TierSponsorship),
			},
			pacing:     stubPacing{behind: true},
			wantType:   DecisionDirect,
			wantLineID: "li-spon",
			wantReason: "sponsorship-wins",
		},
		{
			name: "guaranteed behind pace wins over house",
			items: []publisheradserver.PublisherLineItem{
				active("li-house", "pub-1", publisheradserver.TierHouse),
				active("li-guar", "pub-1", publisheradserver.TierGuaranteed),
			},
			pacing:     stubPacing{behind: true},
			wantType:   DecisionDirect,
			wantLineID: "li-guar",
			wantReason: "guaranteed-behind-pace",
		},
		{
			name: "guaranteed on-pace defers to programmatic",
			items: []publisheradserver.PublisherLineItem{
				active("li-guar", "pub-1", publisheradserver.TierGuaranteed),
			},
			pacing:     stubPacing{behind: false},
			wantType:   DecisionProgrammatic,
			wantReason: "no-direct-winner",
		},
		{
			name: "paused sponsorship skipped",
			items: []publisheradserver.PublisherLineItem{
				{
					ID:           "li-spon-paused",
					PublisherID:  "pub-1",
					PriorityTier: publisheradserver.TierSponsorship,
					Status:       publisheradserver.StatusPaused,
				},
				active("li-guar", "pub-1", publisheradserver.TierGuaranteed),
			},
			pacing:     stubPacing{behind: true},
			wantType:   DecisionDirect,
			wantLineID: "li-guar",
			wantReason: "guaranteed-behind-pace",
		},
		{
			name: "out-of-flight sponsorship skipped",
			items: []publisheradserver.PublisherLineItem{
				{
					ID:            "li-spon-future",
					PublisherID:   "pub-1",
					PriorityTier:  publisheradserver.TierSponsorship,
					Status:        publisheradserver.StatusActive,
					DeliveryStart: ptrTime(now.Add(24 * time.Hour)),
				},
			},
			pacing:     stubPacing{behind: true},
			wantType:   DecisionProgrammatic,
			wantReason: "no-direct-line-items",
		},
		{
			name: "wrong publisher excluded",
			items: []publisheradserver.PublisherLineItem{
				active("li-other-pub", "pub-2", publisheradserver.TierSponsorship),
			},
			pacing:     stubPacing{behind: true},
			wantType:   DecisionProgrammatic,
			wantReason: "no-direct-line-items",
		},
		{
			name: "placement allowlist excludes non-matching",
			items: []publisheradserver.PublisherLineItem{
				{
					ID:           "li-other-pl",
					PublisherID:  "pub-1",
					PriorityTier: publisheradserver.TierSponsorship,
					Status:       publisheradserver.StatusActive,
					PlacementIDs: []string{"pl-other"},
				},
			},
			pacing:     stubPacing{behind: true},
			wantType:   DecisionProgrammatic,
			wantReason: "no-direct-line-items",
		},
		{
			name: "house tier not picked in primary pass",
			items: []publisheradserver.PublisherLineItem{
				active("li-house", "pub-1", publisheradserver.TierHouse),
			},
			pacing:     stubPacing{behind: true},
			wantType:   DecisionProgrammatic,
			wantReason: "no-direct-winner",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(req, tc.items, tc.pacing)
			if got.Type != tc.wantType {
				t.Fatalf("Type: got %v, want %v (reason=%q)", got.Type, tc.wantType, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason: got %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.wantLineID != "" {
				if got.LineItem == nil {
					t.Fatalf("LineItem: got nil, want id=%q", tc.wantLineID)
				}
				if got.LineItem.ID != tc.wantLineID {
					t.Errorf("LineItem.ID: got %q, want %q", got.LineItem.ID, tc.wantLineID)
				}
			}
		})
	}
}

func TestDecideHouse(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	req := Request{PublisherID: "pub-1", PlacementID: "pl-1", Now: now}

	t.Run("house line item picked when present", func(t *testing.T) {
		items := []publisheradserver.PublisherLineItem{
			active("li-house", "pub-1", publisheradserver.TierHouse),
		}
		got := DecideHouse(req, items)
		if got.Type != DecisionDirect || got.LineItem == nil || got.LineItem.ID != "li-house" {
			t.Fatalf("DecideHouse: got %+v", got)
		}
		if got.Reason != "house-fallback" {
			t.Errorf("Reason: got %q, want house-fallback", got.Reason)
		}
	})

	t.Run("no house → DecisionHouse with nil line item", func(t *testing.T) {
		items := []publisheradserver.PublisherLineItem{
			active("li-guar", "pub-1", publisheradserver.TierGuaranteed),
		}
		got := DecideHouse(req, items)
		if got.Type != DecisionHouse {
			t.Fatalf("Type: got %v, want DecisionHouse", got.Type)
		}
		if got.LineItem != nil {
			t.Errorf("LineItem: got %+v, want nil", got.LineItem)
		}
	})
}

func ptrTime(t time.Time) *time.Time { return &t }
