package main

import "testing"

func TestSampleTrace(t *testing.T) {
	// Deterministic: same trace → same decision.
	if sampleTrace("trace-abc", 0.5) != sampleTrace("trace-abc", 0.5) {
		t.Error("sampleTrace not deterministic for the same trace")
	}
	// ratio 1.0 → always in; 0.0 → never in.
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !sampleTrace(id, 1.0) {
			t.Errorf("ratio 1.0 should include %q", id)
		}
		if sampleTrace(id, 0.0) {
			t.Errorf("ratio 0.0 should exclude %q", id)
		}
	}
	// Roughly proportional: ~10% of many traces at ratio 0.1.
	in := 0
	for i := 0; i < 10000; i++ {
		if sampleTrace(string(rune(i))+"-x", 0.1) {
			in++
		}
	}
	if in < 700 || in > 1300 {
		t.Errorf("ratio 0.1 sampled %d/10000, want ~1000", in)
	}
}
