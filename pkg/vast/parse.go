// Parsing + tracker injection for VAST documents that arrive from OUTSIDE —
// a buyer's bid.adm (OpenRTB 2.6 §4.3 carries VAST XML for video/audio).
// The exchange-side consumer (publisher-adserver) parses the buyer's
// document, APPENDS the platform's signed trackers alongside the buyer's own
// (never replacing them — the buyer tracks its delivery, we track billing),
// and re-emits as VAST 4.2. That layering is the standard exchange behaviour
// the Wrapper element formalises; for inline adm we inject directly.
package vast

import (
	"encoding/xml"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrNotVAST: the payload isn't a VAST document (wrong root / not XML).
	ErrNotVAST = errors.New("vast: not a VAST document")
	// ErrVersion: a VAST root with a version outside the supported [2.0, 4.2]
	// range (bid.protocol should have prevented this reaching us).
	ErrVersion = errors.New("vast: unsupported VAST version")
)

// Sniff reports whether a bid.adm payload LOOKS like a VAST document —
// cheap prefix check before committing to a full Parse. DAAST is
// deliberately excluded (retired format; audio rides VAST 4.x here).
func Sniff(adm string) bool {
	s := strings.TrimSpace(adm)
	if strings.HasPrefix(s, "<?xml") {
		if i := strings.Index(s, "?>"); i >= 0 {
			s = strings.TrimSpace(s[i+2:])
		}
	}
	return strings.HasPrefix(s, "<VAST")
}

// Parse unmarshals an external VAST document into the shared struct tree.
// Version tolerance: 2.0–4.2 inclusive (the AdCOM protocol ids we accept on
// bids); the caller re-emits as 4.2 via BuildDocument, which is fine — we
// own the re-marshal and the struct tree is a superset of the older shapes.
// Absent xsi attrs / UniversalAdId are tolerated on INPUT (mandatory only
// when WE emit 4.x InLine ads).
func Parse(b []byte) (*VAST, error) {
	var doc VAST
	if err := xml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotVAST, err)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(doc.Version), 64)
	if err != nil {
		return nil, fmt.Errorf("%w: version %q", ErrVersion, doc.Version)
	}
	if v < 2.0 || v > 4.2 {
		return nil, fmt.Errorf("%w: version %q", ErrVersion, doc.Version)
	}
	return &doc, nil
}

// InjectLinearTrackers appends the PLATFORM's trackers to every ad in the
// document while preserving the buyer's own:
//
//   - Impressions / Errors append at the InLine level.
//   - Event trackers merge into each linear creative's TrackingEvents.
//   - ClickTracking appends; ClickThrough is set ONLY when the buyer left it
//     empty — never clobber the advertiser's landing chain. When the buyer
//     has its own ClickThrough, our signed click URL (which records then
//     302s) rides as an additional ClickTracking pixel instead.
//
// Idempotent per-URI: a URI already present in the target list is skipped,
// so re-injection (an adm that somehow already carries our beacons) never
// double-fires. Wrapper ads are left untouched until wrapper support lands.
func (d *VAST) InjectLinearTrackers(t LinearTrackers, errorURLs []string, click ClickSpec) {
	for i := range d.Ads {
		in := d.Ads[i].InLine
		if in == nil {
			continue
		}
		for _, u := range t.Impression {
			if !hasImpressionURI(in.Impressions, u) {
				in.Impressions = append(in.Impressions, Impression{URI: u})
			}
		}
		for _, u := range errorURLs {
			if !hasErrorURI(in.Errors, u) {
				in.Errors = append(in.Errors, Error{URI: u})
			}
		}
		events := t
		events.Impression = nil // handled above at InLine level
		for c := range in.Creatives.Creatives {
			cr := &in.Creatives.Creatives[c]
			if cr.Linear == nil {
				continue
			}
			if te := buildTrackingEvents(events); te != nil {
				if cr.Linear.TrackingEvents == nil {
					cr.Linear.TrackingEvents = &TrackingEvents{}
				}
				for _, tr := range te.Tracking {
					if !hasTracking(cr.Linear.TrackingEvents.Tracking, tr) {
						cr.Linear.TrackingEvents.Tracking = append(cr.Linear.TrackingEvents.Tracking, tr)
					}
				}
			}
			if click.ClickThrough == "" && len(click.ClickTracking) == 0 {
				continue
			}
			if cr.Linear.VideoClicks == nil {
				cr.Linear.VideoClicks = &VideoClicks{}
			}
			vc := cr.Linear.VideoClicks
			clickTrackers := click.ClickTracking
			if click.ClickThrough != "" {
				if vc.ClickThrough == nil || vc.ClickThrough.URI == "" {
					vc.ClickThrough = &ClickURL{URI: click.ClickThrough}
				} else if vc.ClickThrough.URI != click.ClickThrough {
					// Buyer owns the landing chain — our recording click URL
					// becomes a tracking pixel instead.
					clickTrackers = append(append([]string{}, clickTrackers...), click.ClickThrough)
				}
			}
			for _, u := range clickTrackers {
				if !hasClickURI(vc.ClickTracking, u) {
					vc.ClickTracking = append(vc.ClickTracking, ClickURL{URI: u})
				}
			}
		}
	}
}

// BuildDocument marshals pre-built Ads (spec-built, parsed-and-injected, or
// a mix — pod slots can differ) into one normalized VAST 4.2 document with
// the root attrs strict players require. The shared tail of BuildLinearAd /
// BuildPod and the publisher-adserver's adm path.
func BuildDocument(ads []Ad) ([]byte, error) {
	return marshalDoc(VAST{
		XMLNSXSI: xsiNamespace,
		XSILoc:   xsdLocation,
		Version:  Version,
		Ads:      ads,
	})
}

func hasImpressionURI(list []Impression, uri string) bool {
	for _, x := range list {
		if strings.TrimSpace(x.URI) == uri {
			return true
		}
	}
	return false
}

func hasErrorURI(list []Error, uri string) bool {
	for _, x := range list {
		if strings.TrimSpace(x.URI) == uri {
			return true
		}
	}
	return false
}

func hasClickURI(list []ClickURL, uri string) bool {
	for _, x := range list {
		if strings.TrimSpace(x.URI) == uri {
			return true
		}
	}
	return false
}

func hasTracking(list []Tracking, tr Tracking) bool {
	for _, x := range list {
		if x.Event == tr.Event && strings.TrimSpace(x.URI) == strings.TrimSpace(tr.URI) {
			return true
		}
	}
	return false
}
