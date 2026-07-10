package dash

import (
	"strings"
	"testing"
)

func rep(id string, bw int, segs ...string) Representation {
	sl := &SegmentList{Timescale: 1, Duration: 6, Initialization: &Initialization{SourceURL: "init.mp4"}}
	for _, s := range segs {
		sl.SegmentURLs = append(sl.SegmentURLs, SegmentURL{Media: s})
	}
	return Representation{ID: id, Bandwidth: bw, Codecs: "avc1.4d401e", Width: 640, Height: 360, SegmentList: sl}
}

func vset(reps ...Representation) AdaptationSet {
	return AdaptationSet{MimeType: "video/mp4", ContentType: "video", SegmentAlignment: true, Representations: reps}
}

func TestMPDRoundTrip(t *testing.T) {
	m := NewVOD()
	m.MediaPresentationDuration = Duration(18)
	m.Periods = []Period{
		{ID: "content-0", Duration: Duration(6), AdaptationSets: []AdaptationSet{vset(rep("360p", 928000, "c0.m4s"))}},
		{ID: "ad-0", Duration: Duration(6), AdaptationSets: []AdaptationSet{vset(rep("360p", 928000, "ad0.m4s", "ad1.m4s"))}},
		{ID: "content-1", Duration: Duration(6), AdaptationSets: []AdaptationSet{vset(rep("360p", 928000, "c1.m4s"))}},
	}

	out, err := m.XML()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(out, "<?xml") || !strings.Contains(out, "urn:mpeg:dash:schema:mpd:2011") {
		t.Errorf("MPD header/namespace missing:\n%s", out)
	}
	if !IsMPD(out) {
		t.Error("IsMPD false for a real MPD")
	}

	back, err := ParseMPD(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(back.Periods) != 3 {
		t.Fatalf("want 3 periods, got %d", len(back.Periods))
	}
	if back.Periods[1].ID != "ad-0" {
		t.Errorf("period 1 id = %q, want ad-0", back.Periods[1].ID)
	}
	adRep := back.Periods[1].AdaptationSets[0].Representations[0]
	if adRep.SegmentList == nil || len(adRep.SegmentList.SegmentURLs) != 2 {
		t.Fatalf("ad segment list lost: %+v", adRep.SegmentList)
	}
	if adRep.SegmentList.Initialization.SourceURL != "init.mp4" {
		t.Errorf("ad init segment lost: %+v", adRep.SegmentList.Initialization)
	}
	if adRep.SegmentList.SegmentURLs[1].Media != "ad1.m4s" {
		t.Errorf("ad segment url wrong: %+v", adRep.SegmentList.SegmentURLs)
	}
}

func TestDuration(t *testing.T) {
	cases := map[float64]string{6: "PT6S", 18: "PT18S", 6.5: "PT6.5S", 0: "PT0S"}
	for in, want := range cases {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestIsMPD(t *testing.T) {
	if IsMPD("#EXTM3U\n#EXT-X-VERSION:3\n") {
		t.Error("HLS wrongly detected as MPD")
	}
	if !IsMPD(`<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"></MPD>`) {
		t.Error("MPD not detected")
	}
}
