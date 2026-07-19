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
			cc := CampaignConfig{
				ID:          fmt.Sprintf("%s-c%d", acctKey, c),
				AccountID:   acctKey,
				IOId:        ioKey,
				Name:        fmt.Sprintf("BigWorld Adv%03d Campaign %d (%s)", a, c, format),
				BaseBid:     1.5 + float64(n%12)*0.4, // 1.5–5.9, overlapping strata → real competition
				Currency:    "USD",
				DailyBudget: 1000,
				BidModel:    "cpm",
				PacingMode:  "even",
				Status:      "live",
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
