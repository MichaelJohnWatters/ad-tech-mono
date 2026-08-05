package optimise

import (
	"testing"
	"time"
)

// train records n no-bid calls for a DSP on a channel — enough to trip the
// skip rules once n exceeds MinCalls.
func trainNoBids(r *SmartRouter, channel, dsp string, n int) {
	for i := 0; i < n; i++ {
		r.RecordCall(channel, dsp, false, 0, 5*time.Millisecond, false)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Kill-switch: Enabled=false returns the input untouched, learned skips
// and ranking ignored.
func TestSmartRouterKillSwitch(t *testing.T) {
	r := NewSmartRouter()
	trainNoBids(r, "display", "dsp-bad", 30)
	all := []string{"dsp-bad", "dsp-good"}

	if contains(r.SelectDSPs("display", all), "dsp-bad") {
		t.Fatal("precondition: dsp-bad should be skipped with routing enabled")
	}

	k := DefaultKnobs()
	k.Enabled = false
	r.SetKnobs(func() Knobs { return k })
	got := r.SelectDSPs("display", all)
	if len(got) != 2 || got[0] != "dsp-bad" || got[1] != "dsp-good" {
		t.Fatalf("kill-switch must return the input unchanged, got %v", got)
	}
}

// MinCalls knob: raising the sample floor un-skips a DSP whose evidence no
// longer clears it — the knob the config key exchange.routing_min_calls
// now actually drives (it was silently ignored before).
func TestSmartRouterMinCallsKnob(t *testing.T) {
	r := NewSmartRouter()
	trainNoBids(r, "display", "dsp-bad", 30)
	all := []string{"dsp-bad"}

	if contains(r.SelectDSPs("display", all), "dsp-bad") {
		t.Fatal("30 no-bids should skip at the default MinCalls=20")
	}

	k := DefaultKnobs()
	k.MinCalls = 100
	r.SetKnobs(func() Knobs { return k })
	if !contains(r.SelectDSPs("display", all), "dsp-bad") {
		t.Fatal("MinCalls=100 must keep a 30-call DSP in (thin evidence)")
	}
}

// NeverSkip: an allowlisted DSP survives the skip rules — the deal-holder
// protection (deals are evaluated from returned bids, so routing out the
// deal DSP starves the guarantee).
func TestSmartRouterNeverSkip(t *testing.T) {
	r := NewSmartRouter()
	trainNoBids(r, "display", "dsp-deal", 30)

	k := DefaultKnobs()
	k.NeverSkip = map[string]struct{}{"dsp-deal": {}}
	r.SetKnobs(func() Knobs { return k })

	if !contains(r.SelectDSPs("display", []string{"dsp-deal"}), "dsp-deal") {
		t.Fatal("NeverSkip DSP must stay in fan-out despite 0% bid rate")
	}
}

// ε-probe: with ExplorePct=100 every trace re-includes a skipped DSP
// (appended after ranked candidates); with 0 none do; and the decision is
// deterministic per (trace, dsp) so replays behave identically.
func TestSmartRouterExploreProbe(t *testing.T) {
	r := NewSmartRouter()
	trainNoBids(r, "display", "dsp-bad", 30)
	r.RecordCall("display", "dsp-good", true, 2.50, 5*time.Millisecond, false)
	all := []string{"dsp-bad", "dsp-good"}

	k := DefaultKnobs()
	k.ExplorePct = 100
	r.SetKnobs(func() Knobs { return k })
	got := r.SelectDSPsForTrace("display", all, "trace-1")
	if !contains(got, "dsp-bad") {
		t.Fatalf("ExplorePct=100 must always probe the skipped DSP, got %v", got)
	}
	if got[len(got)-1] != "dsp-bad" {
		t.Fatalf("explored DSP must ride at the back of the list, got %v", got)
	}
	// Preview (no trace) shows the steady-state decision — no probe.
	if contains(r.Preview("display", all), "dsp-bad") {
		t.Fatal("Preview must not include exploration probes")
	}

	k.ExplorePct = 0
	r.SetKnobs(func() Knobs { return k })
	if contains(r.SelectDSPsForTrace("display", all, "trace-1"), "dsp-bad") {
		t.Fatal("ExplorePct=0 must never probe")
	}

	// Determinism at a mid probability: same trace → same decision.
	k.ExplorePct = 37
	r.SetKnobs(func() Knobs { return k })
	first := contains(r.SelectDSPsForTrace("display", all, "trace-det"), "dsp-bad")
	for i := 0; i < 10; i++ {
		if contains(r.SelectDSPsForTrace("display", all, "trace-det"), "dsp-bad") != first {
			t.Fatal("explore decision must be deterministic per (trace, dsp)")
		}
	}

	// And the rate is roughly honoured across distinct traces.
	hits := 0
	for i := 0; i < 1000; i++ {
		if exploreTrace(traceN(i), "dsp-bad", 37) {
			hits++
		}
	}
	if hits < 280 || hits > 460 {
		t.Fatalf("explore rate at 37%% over 1000 traces = %d hits, want roughly 370", hits)
	}
}

func traceN(i int) string {
	return "trace-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String()
}

// Recency window: a DSP with a long bad history rehabilitates after ~window
// GOOD calls (the ε-probe's "second chance" made real). With the old
// lifetime-cumulative averages this test never passes — 5,000 bad calls
// would need ~95k good ones to drag the cumulative bid rate over 5%.
func TestSmartRouterRecencyWindowRehabilitates(t *testing.T) {
	r := NewSmartRouter()
	r.SetKnobs(func() Knobs {
		k := DefaultKnobs()
		k.RecencyWindow = 200
		return k
	})
	all := []string{"dsp-good", "dsp-flaky"}

	// A long deadbeat history → skipped.
	trainNoBids(r, "display", "dsp-flaky", 5000)
	if contains(r.SelectDSPs("display", all), "dsp-flaky") {
		t.Fatal("deadbeat DSP should be skipped after warm-up")
	}

	// The DSP recovers: ~2 windows of solid bidding (as ε-probes would
	// deliver over minutes) lifts the ROLLING bid rate back over the skip
	// threshold.
	for i := 0; i < 400; i++ {
		r.RecordCall("display", "dsp-flaky", true, 4.0, 5*time.Millisecond, false)
	}
	if !contains(r.SelectDSPs("display", all), "dsp-flaky") {
		t.Fatal("recovered DSP must rehabilitate within ~recency-window calls")
	}

	// Timeout-rate skip recovers the same way (the slowpoke scenario).
	trainTimeouts := func(n int) {
		for i := 0; i < n; i++ {
			r.RecordCall("display", "dsp-slow", false, 0, 600*time.Millisecond, true)
		}
	}
	trainTimeouts(5000)
	if contains(r.SelectDSPs("display", append(all, "dsp-slow")), "dsp-slow") {
		t.Fatal("chronic-timeout DSP should be skipped")
	}
	for i := 0; i < 400; i++ {
		r.RecordCall("display", "dsp-slow", true, 4.0, 20*time.Millisecond, false)
	}
	if !contains(r.SelectDSPs("display", append(all, "dsp-slow")), "dsp-slow") {
		t.Fatal("fast-again DSP must rehabilitate within ~recency-window calls")
	}
}
