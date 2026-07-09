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

func (p *periodBuild) toPeriod(id string, r RepInfo) Period {
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
	return Period{
		ID:       id,
		Duration: Duration(p.dur),
		AdaptationSets: []AdaptationSet{{
			MimeType: r.MimeType, ContentType: ct, SegmentAlignment: true, Representations: []Representation{rep},
		}},
	}
}

// AssembleVOD builds a multi-period VOD MPD from an ordered segment list. A new
// Period starts at each content↔ad transition and at each init-segment change
// (a segment carrying a new non-empty Init) — exactly the DASH multi-period
// ad-insertion model. Empty-Init segments inherit the current init.
func AssembleVOD(r RepInfo, segs []Seg) *MPD {
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
		m.Periods = append(m.Periods, cur.toPeriod(id, r))
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
