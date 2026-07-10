package dash

import "fmt"

// RepInfo describes the single Representation (one rung) a DASH SSAI stream is
// assembled for. (Multi-rung ABR DASH — several Representations sharing the
// period structure — is a follow-up; a single high-quality rung is a complete,
// playable DASH stream.)
type RepInfo struct {
	ID        string
	Bandwidth int
	Codecs    string
	Width     int
	Height    int
	MimeType  string // "video/mp4" | "audio/mp4"
	AudioRate int    // audio only
}

// Seg is one segment for DASH assembly (format-neutral). Init is the fMP4 init
// URL; per HLS #EXT-X-MAP semantics it is set only on the first segment of an
// init group, and later segments inherit it.
type Seg struct {
	Media    string
	Init     string
	Duration float64
	Ad       bool
}

type periodBuild struct {
	ad   bool
	init string
	segs []Seg
	dur  float64
}

func (p *periodBuild) toPeriod(id string, r RepInfo, quartileEvents bool) Period {
	sl := &SegmentList{Timescale: 1}
	if p.init != "" {
		sl.Initialization = &Initialization{SourceURL: p.init}
	}
	for _, s := range p.segs {
		sl.SegmentURLs = append(sl.SegmentURLs, SegmentURL{Media: s.Media})
	}
	ct := "video"
	if r.MimeType == "audio/mp4" {
		ct = "audio"
	}
	rep := Representation{
		ID: r.ID, Bandwidth: r.Bandwidth, Codecs: r.Codecs,
		Width: r.Width, Height: r.Height, AudioRate: r.AudioRate, SegmentList: sl,
	}
	period := Period{
		ID:       id,
		Duration: Duration(p.dur),
		AdaptationSets: []AdaptationSet{{
			MimeType: r.MimeType, ContentType: ct, SegmentAlignment: true, Representations: []Representation{rep},
		}},
	}
	if p.ad && quartileEvents {
		period.EventStreams = []EventStream{quartileStream(p.dur)}
	}
	return period
}

// quartileStream builds a timed-metadata EventStream for an ad of dur seconds:
// the five VAST marks at their period-relative times (start=0, firstQuartile=
// 25%, midpoint=50%, thirdQuartile=75%, complete=100%). dash.js fires these at
// playback, giving the client a playback-accurate view of the ad's progress.
func quartileStream(dur float64) EventStream {
	marks := []struct {
		name string
		frac float64
	}{
		{"start", 0}, {"firstQuartile", 0.25}, {"midpoint", 0.5}, {"thirdQuartile", 0.75}, {"complete", 1},
	}
	// Timescale 1000 (ms), not 1 (whole seconds): int-second truncation would put
	// firstQuartile of a 15s ad at 3s instead of 3.75s, disagreeing with the HLS
	// X-QUARTILES offsets (sub-second ftoa). Millisecond ticks keep the two formats
	// consistent.
	es := EventStream{SchemeIDURI: QuartileScheme, Timescale: 1000}
	for i, m := range marks {
		es.Events = append(es.Events, Event{
			PresentationTime: int(m.frac * dur * 1000),
			ID:               fmt.Sprintf("%d", i),
			Body:             m.name,
		})
	}
	return es
}

// AssembleVOD builds a multi-period VOD MPD from an ordered segment list. A new
// Period starts at each content↔ad transition and at each init-segment change
// (a segment carrying a new non-empty Init) — exactly the DASH multi-period
// ad-insertion model. Empty-Init segments inherit the current init. When
// quartileEvents is set, each ad Period gets a timed-metadata EventStream of the
// VAST quartile marks (dash.js fires them at playback for client-side attribution).
func AssembleVOD(r RepInfo, segs []Seg, quartileEvents bool) *MPD {
	m := NewVOD()
	var total float64
	var cur *periodBuild
	adN, contentN := 0, 0

	flush := func() {
		if cur == nil {
			return
		}
		id := fmt.Sprintf("content-%d", contentN)
		if cur.ad {
			id = fmt.Sprintf("ad-%d", adN)
			adN++
		} else {
			contentN++
		}
		m.Periods = append(m.Periods, cur.toPeriod(id, r, quartileEvents))
		cur = nil
	}

	currentInit := ""
	for _, s := range segs {
		segInit := s.Init
		if segInit == "" {
			segInit = currentInit
		} else {
			currentInit = segInit
		}
		boundary := cur == nil || s.Ad != cur.ad || (cur.init != "" && segInit != cur.init)
		if boundary {
			flush()
			cur = &periodBuild{ad: s.Ad, init: segInit}
		}
		cur.segs = append(cur.segs, Seg{Media: s.Media, Duration: s.Duration, Ad: s.Ad})
		cur.dur += s.Duration
		total += s.Duration
	}
	flush()

	m.MediaPresentationDuration = Duration(total)
	return m
}
