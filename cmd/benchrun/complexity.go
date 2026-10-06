package main

import (
	"fmt"
	"strings"
)

// complexity.go derives a human-readable COST PROFILE for a given config —
// what each knob does to the per-cycle work, which dimension dominates, and a
// qualitative weight. It's the "why is this fast/slow" companion to the raw
// auctions/sec number: the GUI shows it live as you turn the knobs, before you
// even hit Run. Everything here is computed from the SAME cost model the real
// cycle() uses (bid loop is O(campaigns), targeting.Evaluate cost scales with
// dimensions, the auction scales with bids), so the numbers line up with what
// a run actually measures.

// ComplexityReport is the structured cost profile the GUI renders.
type ComplexityReport struct {
	Rows      []complexityRow `json:"rows"`      // per-knob cost interpretation
	Dominant  string          `json:"dominant"`  // which dimension bounds throughput
	Summary   string          `json:"summary"`   // one-line "work per cycle" headline
	Formula   string          `json:"formula"`   // combined per-cycle Big-O formula
	Legend    string          `json:"legend"`    // what the symbols mean
	Predicate int64           `json:"predicate"` // targeting predicate checks / cycle (est.)
	Creatives int64           `json:"creatives"` // creative-size scans / cycle (worst case)
	Eligible  int             `json:"eligible"`  // bids that reach the auction (est.)
	SimIO     string          `json:"sim_io"`    // simulated I/O blocking per cycle
}

type complexityRow struct {
	Field  string `json:"field"`  // the knob
	Value  string `json:"value"`  // its current value
	BigO   string `json:"bigo"`   // the knob's Big-O contribution
	Detail string `json:"detail"` // what that costs
	Weight string `json:"weight"` // light | moderate | heavy | extreme | off
}

// legend defines the Big-O symbols used across every row + the combined formula.
const bigOLegend = "C = campaigns · D = targeting dims · K = creatives/campaign · B = eligible bids · S = slots · N = network I/O stages · L = I/O latency · P = parallel workers"

// targetingDims reports how many targeting dimensions each depth gates on —
// the count MUST track buildCampaigns' switch (geo/device/segments/… per level)
// because targeting.Evaluate pays per dimension. none=0, broad=2, dense=5
// (4 include + 1 exclude), extreme=9 (8 include + 1 exclude).
func targetingDims(depth string) (int, string) {
	switch depth {
	case "none":
		return 0, "match-all — no predicates evaluated"
	case "broad":
		return 2, "geo + device"
	case "extreme":
		return 9, "geo, device, segments, categories, keywords, OS, inventory, domains + 1 exclude"
	default: // dense
		return 5, "geo, device, segments, categories + 1 exclude"
	}
}

// slotSizePresent reports whether the requested display slot size exists among
// the creatives each campaign carries — the primary is always 300x250, the
// rest cycle altSizes. If it's absent, NO campaign size-matches and the book
// yields zero bids (a cheap but empty run — worth flagging).
func slotSizePresent(w, h, creativesPer int) bool {
	if w == 300 && h == 250 {
		return true // the primary creative
	}
	for k := 1; k < creativesPer; k++ {
		s := altSizes[(k-1)%len(altSizes)]
		if s[0] == w && s[1] == h {
			return true
		}
	}
	return false
}

func weigh(n int64, moderate, heavy, extreme int64) string {
	switch {
	case n >= extreme:
		return "extreme"
	case n >= heavy:
		return "heavy"
	case n >= moderate:
		return "moderate"
	default:
		return "light"
	}
}

// complexity builds the cost profile for a (already-normalized) Profile.
func complexity(p Profile) ComplexityReport {
	dims, dimsLabel := targetingDims(p.Targeting)

	// DSP bid loop walks the whole book every cycle (O(campaigns)); each campaign
	// pays ~dims predicate checks (short-circuits on first miss, so this is an
	// upper bound) + up-to-creativesPer size scans on display.
	predicate := int64(p.Campaigns) * int64(dims)
	creativeScans := int64(p.Campaigns)
	if p.Channel == "display" {
		creativeScans *= int64(p.CreativesPer)
	}

	// Eligible bids reaching the auction: matchRate% of the book clears geo
	// (none depth → all match), THEN the display size filter (all campaigns
	// share one size set, so it's all-or-nothing), THEN the bid cap.
	matchFrac := float64(p.MatchRate) / 100
	if p.Targeting == "none" {
		matchFrac = 1
	}
	sizeOK := true
	if p.Channel == "display" {
		sizeOK = slotSizePresent(p.SlotW, p.SlotH, p.CreativesPer)
	}
	eligible := 0
	if sizeOK {
		eligible = int(float64(p.Campaigns) * matchFrac)
	}
	if eligible > p.Bids {
		eligible = p.Bids
	}

	// Per-knob Big-O terms (also composed into the combined formula below).
	targetingBigO := "O(C·D)"
	if dims == 0 {
		targetingBigO = "O(C)" // match-all still scans the book, no predicates
	}
	creativeBigO := "O(C)" // non-display: primary creative, no size scan
	if p.Channel == "display" {
		creativeBigO = "O(C·K)"
	}
	auctionBigO := "O(B)" // single-winner / timeslot: one linear pass
	if p.Channel == "retail" {
		auctionBigO = "O(B·log B)" // relevance-weighted: score + sort
	}

	var rows []complexityRow

	// Campaigns — the headline cost lever (bid loop is O(n)).
	rows = append(rows, complexityRow{
		Field: "Campaigns", Value: commas(int64(p.Campaigns)), BigO: "O(C)",
		Detail: fmt.Sprintf("DSP bid loop is O(C): all %s evaluated every cycle. The main throughput lever.", commas(int64(p.Campaigns))),
		Weight: weigh(int64(p.Campaigns), 2_000, 20_000, 200_000),
	})

	// Targeting depth — predicate checks per campaign.
	tWeight := "light"
	switch p.Targeting {
	case "dense":
		tWeight = "moderate"
	case "extreme":
		tWeight = "heavy"
	}
	rows = append(rows, complexityRow{
		Field: "Targeting", Value: fmt.Sprintf("%s (%d dims)", p.Targeting, dims), BigO: targetingBigO,
		Detail: fmt.Sprintf("%s → ~%s predicate checks/cycle (%d dims × %s campaigns).", dimsLabel, commas(predicate), dims, commas(int64(p.Campaigns))),
		Weight: tWeight,
	})

	// Creatives per campaign — size-scan cost on display.
	crDetail := fmt.Sprintf("%d variant(s)/campaign.", p.CreativesPer)
	if p.Channel == "display" {
		crDetail = fmt.Sprintf("%d variant(s) × %s campaigns → up to %s size scans/cycle to match %dx%d.",
			p.CreativesPer, commas(int64(p.Campaigns)), commas(creativeScans), p.SlotW, p.SlotH)
		if !sizeOK {
			crDetail += " ⚠ No creative is " + fmt.Sprintf("%dx%d", p.SlotW, p.SlotH) + " → ZERO bids (empty run)."
		}
	} else {
		crDetail += " Non-display → primary creative, no size scan."
	}
	rows = append(rows, complexityRow{
		Field: "Creatives", Value: fmt.Sprintf("%d", p.CreativesPer), BigO: creativeBigO,
		Detail: crDetail,
		Weight: weigh(creativeScans, 20_000, 200_000, 2_000_000),
	})

	// Match rate → eligible bids.
	rows = append(rows, complexityRow{
		Field: "Match rate", Value: fmt.Sprintf("%d%%", p.MatchRate), BigO: "O(1)",
		Detail: fmt.Sprintf("~%s campaigns clear targeting → %s eligible bids reach the auction (cap %s). Sets B — no asymptotic effect.",
			commas(int64(float64(p.Campaigns)*matchFrac)), commas(int64(eligible)), commas(int64(p.Bids))),
		Weight: "light",
	})

	// Bids + auction strategy — the auction-side cost.
	var auctionDetail string
	switch p.Channel {
	case "retail":
		auctionDetail = fmt.Sprintf("relevance-weighted multi-winner: score + sort %d bids, fill %d slots (O(bids log bids)).", eligible, p.Slots)
	case "dooh":
		auctionDetail = fmt.Sprintf("timeslot → single-winner over %d bids (O(bids)).", eligible)
	default:
		auctionDetail = fmt.Sprintf("single-winner %s: one linear pass over %d bids (O(bids)).", p.PriceMode, eligible)
	}
	auctionRowBigO := auctionBigO
	if p.Separation {
		auctionDetail += " + competitive separation (extra pass to dedupe advertiser/category)."
		auctionRowBigO += " + O(B) sep"
	}
	rows = append(rows, complexityRow{
		Field: "Bids / auction", Value: fmt.Sprintf("%d → %s", eligible, strategyShort(p.Channel)), BigO: auctionRowBigO,
		Detail: auctionDetail,
		Weight: weigh(int64(eligible), 100, 1_000, 4_000),
	})

	// Concurrency.
	rows = append(rows, complexityRow{
		Field: "Concurrency", Value: fmt.Sprintf("%d workers", p.Concurrency), BigO: "÷P",
		Detail: "Parallel workers driving the loop — divides wall-clock. Under simulated I/O, more workers overlap their waits (async fan-out) and recover throughput.",
		Weight: "light",
	})

	// Shading.
	if p.Shading != "" && p.Shading != "disabled" {
		rows = append(rows, complexityRow{
			Field: "Shading", Value: p.Shading, BigO: "O(B)",
			Detail: "Each bid runs bidshading.ShadedBid against a warm win-rate curve (a small per-bid cost).",
			Weight: "light",
		})
	}

	// Tier-3 modeled stages.
	cpuStages := []string{}
	if p.StageDeals {
		cpuStages = append(cpuStages, "deals (scan 32-deal set)")
	}
	if p.StageIdentity {
		cpuStages = append(cpuStages, "identity (bounded BFS, depth 3)")
	}
	netStages := 0
	if p.StageFreqCap {
		netStages++
	}
	if p.StageBudget {
		netStages++
	}
	if len(cpuStages) > 0 {
		rows = append(rows, complexityRow{
			Field: "Tier-3 (CPU)", Value: fmt.Sprintf("%d on", len(cpuStages)), BigO: "O(1)",
			Detail: "Bounded in-memory work per cycle (constant): " + joinComma(cpuStages) + ".",
			Weight: "moderate",
		})
	}

	// Simulated I/O — the dominant term when present.
	simIO := "none (pure compute)"
	ioWeight := "off"
	if netStages > 0 && p.IOLatencyMs > 0 {
		if p.SerialIO {
			simIO = fmt.Sprintf("%dms (serial: %d stages × %dms)", netStages*p.IOLatencyMs, netStages, p.IOLatencyMs)
			ioWeight = "extreme"
		} else {
			simIO = fmt.Sprintf("%dms (async fan-out: %d %s overlap ≈ slowest)", p.IOLatencyMs, netStages, plural(netStages, "stage", "stages"))
			ioWeight = "heavy"
		}
		ioBigO := "O(1)·L" // async fan-out: one round-trip regardless of stage count
		if p.SerialIO {
			ioBigO = "O(N)·L" // serial: adds up
		}
		rows = append(rows, complexityRow{
			Field: "Simulated I/O", Value: fmt.Sprintf("%dms × %d stage(s)", p.IOLatencyMs, netStages), BigO: ioBigO,
			Detail: "Each cycle blocks on " + simIO + ". This dominates — throughput is now bounded by latency ÷ concurrency, not the book.",
			Weight: ioWeight,
		})
	} else if netStages > 0 {
		rows = append(rows, complexityRow{
			Field: "Tier-3 (net)", Value: fmt.Sprintf("%d on, 0ms", netStages), BigO: "O(N)·L",
			Detail: fmt.Sprintf("%d network stage(s) enabled but Simulated I/O is 0 — set it >0 to model their round-trip cost.", netStages),
			Weight: "off",
		})
	}

	// Dominant-cost verdict: simulated I/O (if any) dwarfs compute; else the
	// bigger of the bid-loop work vs the auction work.
	var dominant string
	switch {
	case netStages > 0 && p.IOLatencyMs > 0:
		dominant = "Simulated I/O — each cycle waits " + simIO + ", which dwarfs the compute. Crank concurrency to overlap the waits."
	case predicate >= int64(eligible)*50 && predicate > 0:
		dominant = fmt.Sprintf("DSP bid loop — O(campaigns) targeting eval (~%s checks/cycle) is the bottleneck. Fewer campaigns or shallower targeting = faster.", commas(predicate))
	case eligible >= 1000:
		dominant = fmt.Sprintf("The auction — %s bids through %s. Lower the bid cap to speed it up.", commas(int64(eligible)), strategyShort(p.Channel))
	default:
		dominant = "Balanced — neither the bid loop nor the auction clearly dominates at this scale."
	}

	summary := fmt.Sprintf("~%s targeting checks + %s creative scans → %s bids → %s auction, ×%d workers.",
		commas(predicate), commas(creativeScans), commas(int64(eligible)), strategyShort(p.Channel), p.Concurrency)

	// Combined per-cycle Big-O: the bid-loop term (C × its per-campaign factors)
	// + the auction term, then the I/O term (if any), all parallelized ÷P.
	// Constants (match rate, Tier-3 CPU) drop out of the asymptotic sum.
	loopFactors := []string{}
	if dims > 0 {
		loopFactors = append(loopFactors, "D")
	}
	if p.Channel == "display" {
		loopFactors = append(loopFactors, "K")
	}
	loopTerm := "C"
	switch len(loopFactors) {
	case 1:
		loopTerm = "C·" + loopFactors[0]
	case 2:
		loopTerm = "C·(" + strings.Join(loopFactors, "+") + ")"
	}
	aucTerm := auctionBigO[2 : len(auctionBigO)-1] // strip the "O(" ")" wrapper
	computeTerms := []string{loopTerm, aucTerm}
	if p.Separation {
		computeTerms = append(computeTerms, "B")
	}
	formula := "O(" + strings.Join(dedupe(computeTerms), " + ") + ") compute"
	if netStages > 0 && p.IOLatencyMs > 0 {
		if p.SerialIO {
			formula += fmt.Sprintf("  +  O(N)·L I/O (serial, %dms)", netStages*p.IOLatencyMs)
		} else {
			formula += fmt.Sprintf("  +  O(1)·L I/O (async, %dms)", p.IOLatencyMs)
		}
	}
	formula += fmt.Sprintf("   ÷ P (%d workers)", p.Concurrency)

	return ComplexityReport{
		Rows: rows, Dominant: dominant, Summary: summary,
		Formula: formula, Legend: bigOLegend,
		Predicate: predicate, Creatives: creativeScans, Eligible: eligible, SimIO: simIO,
	}
}

func strategyShort(channel string) string {
	switch channel {
	case "retail":
		return "relevance-weighted"
	case "dooh":
		return "timeslot"
	default:
		return "single-winner"
	}
}

// dedupe drops repeated asymptotic terms (O(B)+O(B) = O(B)) while preserving order.
func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
