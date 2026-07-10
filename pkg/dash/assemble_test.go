package dash

import "testing"

func TestAssembleVODPeriods(t *testing.T) {
	// content(init=c) x2, ad(init=a) x2, content(init=c restored) x1 → 3 periods.
	segs := []Seg{
		{Media: "c0.m4s", Init: "cinit.mp4", Duration: 6},
		{Media: "c1.m4s", Duration: 6}, // inherits content init
		{Media: "ad0.m4s", Init: "ainit.mp4", Duration: 6, Ad: true},
		{Media: "ad1.m4s", Duration: 6, Ad: true},         // inherits ad init
		{Media: "c2.m4s", Init: "cinit.mp4", Duration: 6}, // content init restored
	}
	m := AssembleVOD(RepInfo{ID: "360p", Bandwidth: 928000, Codecs: "avc1.4d401e", Width: 640, Height: 360, MimeType: "video/mp4"}, segs, false)

	if len(m.Periods) != 3 {
		t.Fatalf("want 3 periods (content/ad/content), got %d", len(m.Periods))
	}
	if m.Periods[0].ID != "content-0" || m.Periods[1].ID != "ad-0" || m.Periods[2].ID != "content-1" {
		t.Errorf("period ids = %s/%s/%s", m.Periods[0].ID, m.Periods[1].ID, m.Periods[2].ID)
	}
	// Content period 0: 2 segments, content init.
	sl0 := m.Periods[0].AdaptationSets[0].Representations[0].SegmentList
	if len(sl0.SegmentURLs) != 2 || sl0.Initialization.SourceURL != "cinit.mp4" {
		t.Errorf("content period 0 wrong: %+v", sl0)
	}
	// Ad period: 2 segments, ad init.
	slAd := m.Periods[1].AdaptationSets[0].Representations[0].SegmentList
	if len(slAd.SegmentURLs) != 2 || slAd.Initialization.SourceURL != "ainit.mp4" {
		t.Errorf("ad period wrong: %+v", slAd)
	}
	// Restored content period: 1 segment, content init again.
	sl2 := m.Periods[2].AdaptationSets[0].Representations[0].SegmentList
	if len(sl2.SegmentURLs) != 1 || sl2.Initialization.SourceURL != "cinit.mp4" {
		t.Errorf("restored content period wrong: %+v", sl2)
	}
	if m.MediaPresentationDuration != "PT30S" {
		t.Errorf("total duration = %q, want PT30S", m.MediaPresentationDuration)
	}

	// It must serialise + round-trip.
	xml, err := m.XML()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseMPD(xml); err != nil {
		t.Errorf("assembled MPD didn't round-trip: %v", err)
	}
}

func TestAssembleVODQuartileEvents(t *testing.T) {
	// A 10s ad → an EventStream (ms timescale) with the 5 quartile marks at
	// 0/2.5/5/7.5/10s = 0/2500/5000/7500/10000ms. The 2.5s/7.5s marks exercise the
	// sub-second precision that the old whole-second timescale truncated to 2s/7s.
	segs := []Seg{
		{Media: "c0.m4s", Init: "cinit.mp4", Duration: 6},
		{Media: "ad0.m4s", Init: "ainit.mp4", Duration: 6, Ad: true},
		{Media: "ad1.m4s", Duration: 4, Ad: true},
	}
	m := AssembleVOD(RepInfo{ID: "360p", Bandwidth: 928000, MimeType: "video/mp4"}, segs, true)

	// Content period: no events. Ad period: one quartile EventStream.
	if len(m.Periods[0].EventStreams) != 0 {
		t.Errorf("content period should have no events")
	}
	ad := m.Periods[1]
	if len(ad.EventStreams) != 1 {
		t.Fatalf("ad period should have 1 EventStream, got %d", len(ad.EventStreams))
	}
	es := ad.EventStreams[0]
	if es.SchemeIDURI != QuartileScheme {
		t.Errorf("scheme = %q, want %q", es.SchemeIDURI, QuartileScheme)
	}
	// Millisecond timescale so sub-second quartiles land exactly.
	if es.Timescale != 1000 {
		t.Errorf("EventStream timescale = %d, want 1000 (ms)", es.Timescale)
	}
	if len(es.Events) != 5 {
		t.Fatalf("want 5 quartile events, got %d", len(es.Events))
	}
	want := []struct {
		name string
		t    int
	}{{"start", 0}, {"firstQuartile", 2500}, {"midpoint", 5000}, {"thirdQuartile", 7500}, {"complete", 10000}}
	for i, w := range want {
		if es.Events[i].Body != w.name || es.Events[i].PresentationTime != w.t {
			t.Errorf("event %d = %s@%d, want %s@%d", i, es.Events[i].Body, es.Events[i].PresentationTime, w.name, w.t)
		}
	}
	// Off by default: no events when the flag is false.
	if off := AssembleVOD(RepInfo{MimeType: "video/mp4"}, segs, false); len(off.Periods[1].EventStreams) != 0 {
		t.Error("quartileEvents=false must not emit EventStreams")
	}
}
