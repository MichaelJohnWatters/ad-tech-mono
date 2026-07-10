package dash

import "fmt"

// RungInput is one ABR rung's segment list for multi-rung assembly.
type RungInput struct {
	Rep  RepInfo
	Segs []Seg
}

// AssembleMultiRung builds an ABR multi-Representation VOD MPD: each Period holds
// one Representation per rung. All rungs MUST share the same period structure —
// same segment count and the same content/ad + init-change boundaries — which the
// SSAI stitcher guarantees by using the same ad decisions and equal segment
// durations across rungs. Period boundaries are taken from the first rung; each
// rung contributes its own segments (and its own fMP4 init) to every period.
//
// Falls back to single-rung AssembleVOD when only one rung is supplied.
func AssembleMultiRung(rungs []RungInput, quartileEvents bool) *MPD {
	if len(rungs) == 0 {
		return NewVOD()
	}
	if len(rungs) == 1 {
		return AssembleVOD(rungs[0].Rep, rungs[0].Segs, quartileEvents)
	}
	base := rungs[0].Segs
	bounds := periodBounds(base) // [start,end) ranges + ad flag, from the base rung

	// Per-rung running init, so each period picks up the init active at its start.
	inits := make([]string, len(rungs))

	m := NewVOD()
	var total float64
	adN, contentN := 0, 0
	for _, b := range bounds {
		id := fmt.Sprintf("content-%d", contentN)
		if b.ad {
			id = fmt.Sprintf("ad-%d", adN)
			adN++
		} else {
			contentN++
		}
		period := Period{ID: id}
		var periodDur float64
		reps := make([]Representation, 0, len(rungs))
		for ri, rung := range rungs {
			segs := rung.Segs
			sl := &SegmentList{Timescale: 1}
			for i := b.start; i < b.end && i < len(segs); i++ {
				if segs[i].Init != "" {
					inits[ri] = segs[i].Init
				}
				sl.SegmentURLs = append(sl.SegmentURLs, SegmentURL{Media: segs[i].Media})
				if ri == 0 {
					periodDur += segs[i].Duration
				}
			}
			if inits[ri] != "" {
				sl.Initialization = &Initialization{SourceURL: inits[ri]}
			}
			reps = append(reps, representationFor(rung.Rep, sl))
		}
		period.Duration = Duration(periodDur)
		total += periodDur
		ct := "video"
		if rungs[0].Rep.MimeType == "audio/mp4" {
			ct = "audio"
		}
		period.AdaptationSets = []AdaptationSet{{
			MimeType: rungs[0].Rep.MimeType, ContentType: ct, SegmentAlignment: true, Representations: reps,
		}}
		if b.ad && quartileEvents {
			period.EventStreams = []EventStream{quartileStream(periodDur)}
		}
		m.Periods = append(m.Periods, period)
	}
	m.MediaPresentationDuration = Duration(total)
	return m
}

func representationFor(r RepInfo, sl *SegmentList) Representation {
	return Representation{
		ID: r.ID, Bandwidth: r.Bandwidth, Codecs: r.Codecs,
		Width: r.Width, Height: r.Height, AudioRate: r.AudioRate, SegmentList: sl,
	}
}

type bound struct {
	start, end int
	ad         bool
}

// periodBounds computes the [start,end) segment ranges that become Periods: a new
// range at each content↔ad transition and at each init change (a segment with a
// new non-empty Init). Mirrors AssembleVOD's grouping so single- and multi-rung
// produce the same period structure.
func periodBounds(segs []Seg) []bound {
	var out []bound
	currentInit := ""
	start := 0
	curAd := false
	curInit := ""
	open := false
	for i, s := range segs {
		segInit := s.Init
		if segInit == "" {
			segInit = currentInit
		} else {
			currentInit = segInit
		}
		boundary := !open || s.Ad != curAd || (curInit != "" && segInit != curInit)
		if boundary {
			if open {
				out = append(out, bound{start: start, end: i, ad: curAd})
			}
			start, curAd, curInit, open = i, s.Ad, segInit, true
		}
	}
	if open {
		out = append(out, bound{start: start, end: len(segs), ad: curAd})
	}
	return out
}
