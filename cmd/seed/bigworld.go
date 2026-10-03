package main

import (
	"context"
	"fmt"
)

// SeedBigWorld generates a synthetic-but-FUNCTIONAL demand+supply pool ON TOP
// of the curated small world (it never truncates — every insert is idempotent
// via DeriveID + ON CONFLICT). "Functional" is the point: unlike the load-test
// bigworld_test.go, each campaign gets a real display creative and funding, so
// it actually WINS auctions and produces real spend/impressions — the data is
// as real as the small world's; only the names and content are generated.
//
// Big-world advertisers are owned by the INTERNAL DSP so cmd/dsp-internal
// loads and bids their campaigns. Run BEFORE SeedDevUsers + SeedAdvertiser
// balances so the accounts pick up logins (every advertiser/publisher account
// becomes loginable) and starting balances for free.
func (in *inserter) SeedBigWorld(ctx context.Context, advertisers, publishers, campaignsPer, placementsPer int) error {
	internalDSP := DeriveID("dsp", "internal")

	// Rotate over a few geos/devices/formats/bids so the pool has real
	// auction variety without hand-authoring each campaign.
	geoSets := [][]string{{"USA"}, {"GBR"}, {"USA", "CAN"}, {"DEU", "FRA"}, nil}
	devSets := [][]string{{"mobile"}, {"desktop"}, {"mobile", "desktop"}, nil}
	formats := []string{"display", "display", "display", "native", "video"} // display-weighted so most serve immediately
	// Budget SPREAD (not flat): small budgets deplete mid-run — a realistic
	// marketplace has some advertisers exit during the day (visible in
	// budget_depletions + dsp_calls no_bid_reason=budget_depleted) — while
	// the bigger strata keep fill from collapsing on sustained load runs.
	budgets := []float64{150, 300, 500, 1000, 2500}

	campN, plN := 0, 0
	for a := 0; a < advertisers; a++ {
		acctKey := fmt.Sprintf("bigworld-adv-%03d", a)
		ioKey := acctKey + "-io"
		// Account (owned by internal DSP) + its insertion order.
		if err := in.upsertAccounts(ctx,
			map[string]string{acctKey: fmt.Sprintf("BigWorld Advertiser %03d", a)},
			map[string]string{acctKey: internalDSP}); err != nil {
			return fmt.Errorf("bigworld advertiser %d: %w", a, err)
		}
		if err := in.upsertInsertionOrders(ctx, map[string]ioInsertPayload{
			// upsertInsertionOrders uses accountID as a raw UUID (RLS tenant),
			// unlike upsertCampaign which derives from the key. Pass derived.
			ioKey: {external: ioKey, accountID: DeriveID("account", acctKey), name: acctKey + " IO", currency: "USD"},
		}); err != nil {
			return fmt.Errorf("bigworld IO %d: %w", a, err)
		}
		for c := 0; c < campaignsPer; c++ {
			n := a*campaignsPer + c
			format := formats[n%len(formats)]
			baseBid := 1.5 + float64(n%12)*0.4 // 1.5–5.9, overlapping strata → real competition
			// Premium stratum: ~1 in 7 campaigns bids 9–19.5 CPM. Without it
			// the whole market topped out at ~$5.9 and premium placements
			// ($12/$18 floors) were DEAD inventory — every DSP declined
			// below_floor on 100% of their auctions. Premium CPM × the small
			// budget strata also means these deplete fastest, which is the
			// realistic degradation arc on sustained load.
			premium := n%7 == 6
			if premium {
				baseBid = 9 + float64(n%4)*3.5 // 9 / 12.5 / 16 / 19.5
			}
			// Bid shading only produces savings on campaigns that actually WIN, and in
			// the big world the winners are the PREMIUM (high-bid) stratum — the low-bid
			// majority loses every auction, so shading them would do nothing. So give the
			// premium campaigns a shading mode, spread across all three, so a LOAD TEST
			// leaves rich, comparable savings data (one arm per mode). The second-price
			// signal (exchange clear_price → curve midpoint) keeps them shading STABLY
			// toward the real clearing under sustained load instead of decaying to zero.
			shadingMode := "disabled"
			if premium {
				shadingMode = []string{"conservative", "moderate", "aggressive"}[n%3]
			}
			cc := CampaignConfig{
				ID:          fmt.Sprintf("%s-c%d", acctKey, c),
				AccountID:   acctKey,
				IOId:        ioKey,
				Name:        fmt.Sprintf("BigWorld Adv%03d Campaign %d (%s)", a, c, format),
				BaseBid:     baseBid,
				Currency:    "USD",
				DailyBudget: budgets[n%len(budgets)],
				BidModel:    "cpm",
				PacingMode:  "even",
				Status:      "live",
				ShadingMode: shadingMode,
				Format:      format,
				// A real display creative so the campaign SERVES (a bare
				// campaign silently no-bids — the exact trap the small
				// world's placeholders avoid). Non-display formats also
				// carry a display fallback creative so they still win the
				// display auctions the simulator generates.
				// Multi-size so big-world campaigns compete across the full
				// display size mix (single-size 300x250 left other-size
				// auctions with thin/no competition). Covers every seeded
				// placement size incl. 336x280.
				Creatives: []CreativeYAML{
					{Width: 300, Height: 250}, {Width: 728, Height: 90},
					{Width: 160, Height: 600}, {Width: 320, Height: 50},
					{Width: 300, Height: 600}, {Width: 970, Height: 250},
					{Width: 336, Height: 280},
				},
				Targeting: &TargetingYAML{
					Include: TargetingSetYAML{
						Geo:    geoSets[n%len(geoSets)],
						Device: devSets[n%len(devSets)],
					},
				},
			}
			// Premium buyers buy premium-shaped slices, not the whole market:
			// broad-targeted 9-19.5 CPM campaigns won EVERYTHING they matched
			// (first soak with the stratum: one premium account took 17k
			// impressions while the standard demo advertisers won ZERO —
			// terrible demo portals and unrealistic concentration). Narrow
			// them to USA+desktop: they still clear the $12/$18 premium
			// floors on that slice, and the 1.5-5.9 market keeps the rest.
			if premium {
				cc.Targeting.Include.Geo = []string{"USA"}
				cc.Targeting.Include.Device = []string{"desktop"}
			}
			if err := in.upsertCampaign(ctx, cc); err != nil {
				return fmt.Errorf("bigworld campaign %s: %w", cc.ID, err)
			}
			campN++
		}
	}

	for p := 0; p < publishers; p++ {
		pubKey := fmt.Sprintf("bigworld-pub-%03d", p)
		pub := PublisherYAML{
			ID:          pubKey,
			Name:        fmt.Sprintf("BigWorld Publisher %03d", p),
			Domain:      fmt.Sprintf("bw-pub-%03d.example", p),
			Currency:    "USD",
			RevSharePct: 20 + (p%3)*5, // 20/25/30 — revshare variety
		}
		for pl := 0; pl < placementsPer; pl++ {
			pub.Placements = append(pub.Placements, PlacementYAML{
				ID:         fmt.Sprintf("%s-pl%d", pubKey, pl),
				Name:       fmt.Sprintf("BigWorld Pub%03d Slot %d", p, pl),
				Format:     "display",
				Width:      300,
				Height:     250,
				FloorPrice: 0.5 + float64(pl%3)*0.25,
				PageURL:    fmt.Sprintf("https://bw-pub-%03d.example/page%d", p, pl),
			})
		}
		if err := in.upsertPublisher(ctx, pub); err != nil {
			return fmt.Errorf("bigworld publisher %d: %w", p, err)
		}
		plN += len(pub.Placements)
	}

	in.log.Info("seeded big world (additive)",
		"advertisers", advertisers, "campaigns", campN,
		"publishers", publishers, "placements", plN)
	fmt.Printf("big world: +%d advertisers / %d campaigns, +%d publishers / %d placements\n",
		advertisers, campN, publishers, plN)
	return nil
}
