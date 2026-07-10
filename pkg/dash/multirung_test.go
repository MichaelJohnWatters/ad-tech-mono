package dash

import "testing"

func TestAssembleMultiRung(t *testing.T) {
	// Two rungs, identical structure: content(init) / ad(init) / content(init).
	mk := func(prefix, cinit, ainit string) []Seg {
		return []Seg{
			{Media: prefix + "_c0.m4s", Init: cinit, Duration: 6},
			{Media: prefix + "_ad0.m4s", Init: ainit, Duration: 6, Ad: true},
			{Media: prefix + "_ad1.m4s", Duration: 6, Ad: true},
			{Media: prefix + "_c1.m4s", Init: cinit, Duration: 6},
		}
	}
	rungs := []RungInput{
		{Rep: RepInfo{ID: "360p", Bandwidth: 928000, Width: 640, Height: 360, MimeType: "video/mp4"}, Segs: mk("360", "c360.mp4", "a360.mp4")},
		{Rep: RepInfo{ID: "720p", Bandwidth: 2928000, Width: 1280, Height: 720, MimeType: "video/mp4"}, Segs: mk("720", "c720.mp4", "a720.mp4")},
	}
	m := AssembleMultiRung(rungs, true)

	// content / ad / content = 3 periods.
	if len(m.Periods) != 3 {
		t.Fatalf("want 3 periods, got %d", len(m.Periods))
	}
	if m.Periods[1].ID != "ad-0" {
		t.Errorf("period 1 id = %q, want ad-0", m.Periods[1].ID)
	}
	// Every period carries BOTH rungs as Representations.
	for i, p := range m.Periods {
		reps := p.AdaptationSets[0].Representations
		if len(reps) != 2 {
			t.Fatalf("period %d has %d reps, want 2", i, len(reps))
		}
		if reps[0].ID != "360p" || reps[1].ID != "720p" {
			t.Errorf("period %d rep ids = %s/%s", i, reps[0].ID, reps[1].ID)
		}
	}
	// Ad period: each rung uses its OWN ad init + 2 segments; quartile events present.
	ad := m.Periods[1]
	if ad.AdaptationSets[0].Representations[0].SegmentList.Initialization.SourceURL != "a360.mp4" {
		t.Errorf("360 ad init wrong: %+v", ad.AdaptationSets[0].Representations[0].SegmentList.Initialization)
	}
	if ad.AdaptationSets[0].Representations[1].SegmentList.Initialization.SourceURL != "a720.mp4" {
		t.Errorf("720 ad init wrong")
	}
	if n := len(ad.AdaptationSets[0].Representations[1].SegmentList.SegmentURLs); n != 2 {
		t.Errorf("720 ad segs = %d, want 2", n)
	}
	if len(ad.EventStreams) != 1 || ad.EventStreams[0].SchemeIDURI != QuartileScheme {
		t.Errorf("ad period missing quartile EventStream")
	}
	// Content period after the ad restores each rung's content init.
	c1 := m.Periods[2].AdaptationSets[0].Representations[1].SegmentList
	if c1.Initialization.SourceURL != "c720.mp4" {
		t.Errorf("restored 720 content init wrong: %+v", c1.Initialization)
	}

	// Round-trips + total duration = 24s.
	xml, err := m.XML()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseMPD(xml); err != nil {
		t.Errorf("multi-rung MPD didn't round-trip: %v", err)
	}
	if m.MediaPresentationDuration != "PT24S" {
		t.Errorf("total duration = %q, want PT24S", m.MediaPresentationDuration)
	}

	// One rung falls back to the single-rep path.
	if one := AssembleMultiRung(rungs[:1], false); len(one.Periods[1].AdaptationSets[0].Representations) != 1 {
		t.Error("single rung should produce single-representation periods")
	}
}
