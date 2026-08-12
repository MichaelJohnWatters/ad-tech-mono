// Package ara implements the SERVER side of the Privacy Sandbox Attribution
// Reporting API (ARA): building the standards-correct registration headers a
// browser reads on an ad impression (a "source") and on a conversion (a
// "trigger"), and parsing the noised/aggregated reports the browser later POSTs
// back to the reporting origin.
//
// # Scope / mock boundary
//
// See docs/attribution-phase4-ara.md. What is REAL and inspectable here:
//   - the registration header wire format (this package, unit-tested),
//   - the report-ingest endpoints + separate low-resolution storage.
//
// What is NOT here and structurally CANNOT be (only a real Privacy-Sandbox
// browser, at volume, over days, does these):
//   - the source↔trigger MATCH,
//   - the k-anonymity NOISE and the multi-day DELAY,
//   - the aggregation-service decrypt of aggregatable payloads.
//
// ARA is a REPORTING-ONLY overlay: it never bills, and its low-resolution stream
// is stored separately and never joined into the exact conversions table, so the
// platform's exact-money / zero-slippage invariants are untouched. Registration
// is consent-gated by the caller (privacy.Evaluate().Personalise) exactly like
// the deterministic identity path.
package ara

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// NormalizeDestination reduces a raw destination to the ARA canonical form —
// scheme + registrable domain (eTLD+1), with no path, query, or port
// (https://shop.acme.co.uk/x → https://acme.co.uk). This is the shape ARA matches
// a conversion against, and the shape a real browser sends back. Both the ad
// server (which produces the source beacon) and the tracker (which re-validates a
// registration) call this, so a hand-crafted — even validly signed — beacon can't
// record an arbitrary destination, and the stored value can't drift from what a
// browser reports. Returns ("", false) when there is no usable host. Names with no
// public suffix (localhost, a bare IP) fall back to the host so local/dev demos
// still produce a usable value.
func NormalizeDestination(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	s := raw
	if !strings.Contains(s, "://") {
		s = "https://" + s // tolerate a scheme-less input, e.g. "acme.com/x"
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(u.Hostname())
	if err != nil {
		site = u.Hostname()
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + site, true
}

// Response headers a browser reads to register a source / trigger, and the
// request header a Privacy-Sandbox browser sends to mark a request
// attribution-eligible (we only register when it is present, unless a dev flag
// forces it).
const (
	HeaderRegisterSource  = "Attribution-Reporting-Register-Source"
	HeaderRegisterTrigger = "Attribution-Reporting-Register-Trigger"
	HeaderEligible        = "Attribution-Reporting-Eligible"
)

// Well-known paths the browser POSTs reports to on the reporting origin (the
// tracker, here). Debug variants are the same paths under /debug.
const (
	PathEventReport     = "/.well-known/attribution-reporting/report-event-attribution"
	PathAggregateReport = "/.well-known/attribution-reporting/report-aggregate-attribution"
)

// Source lifetime bounds. ARA clamps a source's expiry to [1 day, 30 days] and
// defaults to the max.
const (
	MinSourceExpiry     = 24 * time.Hour
	MaxSourceExpiry     = 30 * 24 * time.Hour
	DefaultSourceExpiry = MaxSourceExpiry
)

// Source is an attribution source registered at impression time. It renders to
// the JSON value of the Attribution-Reporting-Register-Source response header.
type Source struct {
	// SourceEventID is the (low-entropy on the event-level report) source id.
	SourceEventID uint64
	// Destination is the advertiser site as scheme + eTLD+1, e.g.
	// "https://advertiser.example". Required by ARA.
	Destination string
	// Expiry is clamped to [MinSourceExpiry, MaxSourceExpiry]; zero → default.
	Expiry time.Duration
	// Priority breaks ties when several sources match a trigger (higher wins).
	Priority int64
	// AggregationKeys maps a name → a 128-bit hex key piece ("0x159") for the
	// aggregatable histogram; combined with the trigger's key_piece.
	AggregationKeys map[string]string
	// FilterData constrains which triggers may attribute to this source.
	FilterData map[string][]string
	// DebugKey (0 = omit) ties a source to its debug report during development.
	DebugKey uint64
}

// clampExpiry returns the ARA-legal source lifetime in whole seconds.
func (s Source) clampExpiry() int64 {
	d := s.Expiry
	if d <= 0 {
		d = DefaultSourceExpiry
	}
	if d < MinSourceExpiry {
		d = MinSourceExpiry
	}
	if d > MaxSourceExpiry {
		d = MaxSourceExpiry
	}
	return int64(d / time.Second)
}

// MarshalHeader renders the Attribution-Reporting-Register-Source header value.
// ARA requires the 64-bit numeric fields (source_event_id, expiry, priority,
// debug_key) to be JSON STRINGS, so they can't lose precision.
func (s Source) MarshalHeader() (string, error) {
	if s.Destination == "" {
		return "", fmt.Errorf("ara: source destination is required")
	}
	m := map[string]any{
		"source_event_id": strconv.FormatUint(s.SourceEventID, 10),
		"destination":     s.Destination,
		"expiry":          strconv.FormatInt(s.clampExpiry(), 10),
	}
	if s.Priority != 0 {
		m["priority"] = strconv.FormatInt(s.Priority, 10)
	}
	if len(s.AggregationKeys) > 0 {
		m["aggregation_keys"] = s.AggregationKeys
	}
	if len(s.FilterData) > 0 {
		m["filter_data"] = s.FilterData
	}
	if s.DebugKey != 0 {
		m["debug_key"] = strconv.FormatUint(s.DebugKey, 10)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// EventTriggerData is one event-level trigger entry: a coarse trigger_data value
// (the browser masks it to the source's configured bit-width) plus optional
// priority and per-source dedup key.
type EventTriggerData struct {
	TriggerData      uint64
	Priority         int64
	DeduplicationKey uint64 // 0 = omit
}

// AggregatableTriggerData contributes a key_piece to the named source keys when
// building the aggregatable histogram bucket.
type AggregatableTriggerData struct {
	KeyPiece   string   // 128-bit hex, e.g. "0x400"
	SourceKeys []string // names from Source.AggregationKeys
}

// Trigger is a conversion trigger. It renders to the JSON value of the
// Attribution-Reporting-Register-Trigger response header.
type Trigger struct {
	EventTriggerData        []EventTriggerData
	AggregatableTriggerData []AggregatableTriggerData
	AggregatableValues      map[string]int // name → bucket value (a NUMBER in ARA)
	Filters                 map[string][]string
}

// MarshalHeader renders the Attribution-Reporting-Register-Trigger header value.
func (t Trigger) MarshalHeader() (string, error) {
	m := map[string]any{}
	if len(t.EventTriggerData) > 0 {
		etd := make([]map[string]any, 0, len(t.EventTriggerData))
		for _, e := range t.EventTriggerData {
			one := map[string]any{"trigger_data": strconv.FormatUint(e.TriggerData, 10)}
			if e.Priority != 0 {
				one["priority"] = strconv.FormatInt(e.Priority, 10)
			}
			if e.DeduplicationKey != 0 {
				one["deduplication_key"] = strconv.FormatUint(e.DeduplicationKey, 10)
			}
			etd = append(etd, one)
		}
		m["event_trigger_data"] = etd
	}
	if len(t.AggregatableTriggerData) > 0 {
		atd := make([]map[string]any, 0, len(t.AggregatableTriggerData))
		for _, a := range t.AggregatableTriggerData {
			atd = append(atd, map[string]any{"key_piece": a.KeyPiece, "source_keys": a.SourceKeys})
		}
		m["aggregatable_trigger_data"] = atd
	}
	if len(t.AggregatableValues) > 0 {
		m["aggregatable_values"] = t.AggregatableValues
	}
	if len(t.Filters) > 0 {
		m["filters"] = t.Filters
	}
	if len(m) == 0 {
		return "", fmt.Errorf("ara: trigger has no event or aggregatable data")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReportType distinguishes the two report streams the browser sends back.
type ReportType string

const (
	ReportEvent     ReportType = "event"
	ReportAggregate ReportType = "aggregate"
)

// EventReport is the browser-sent event-level report body. Fields are a subset —
// enough to persist + surface the low-resolution attribution; the full body is
// stored raw alongside.
type EventReport struct {
	AttributionDestination string  `json:"attribution_destination"`
	SourceEventID          string  `json:"source_event_id"`
	TriggerData            string  `json:"trigger_data"`
	SourceType             string  `json:"source_type"`
	ReportID               string  `json:"report_id"`
	RandomizedTriggerRate  float64 `json:"randomized_trigger_rate"`
	ScheduledReportTime    string  `json:"scheduled_report_time"`
}

// AggregatableReport is the browser-sent aggregatable report body. The
// aggregation_service_payloads are ENCRYPTED — readable only via the aggregation
// service (the mock boundary). We persist the envelope, never decrypt.
type AggregatableReport struct {
	AttributionDestination     string            `json:"attribution_destination"`
	SourceRegistrationTime     string            `json:"source_registration_time"`
	SharedInfo                 string            `json:"shared_info"`
	AggregationServicePayloads []json.RawMessage `json:"aggregation_service_payloads"`
	// ReportID is the browser's report id, used only as a quarantine dedup key. In
	// a real aggregatable report it lives inside the JSON-encoded shared_info; we
	// also accept it top-level and fall back to parsing shared_info.
	ReportID string `json:"report_id"`
}

// ParseEventReport decodes an event-level report body (strict-ish: unknown
// fields are ignored, but the destination must be present).
func ParseEventReport(body []byte) (*EventReport, error) {
	var r EventReport
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("ara: parse event report: %w", err)
	}
	if r.AttributionDestination == "" {
		return nil, fmt.Errorf("ara: event report missing attribution_destination")
	}
	return &r, nil
}

// ParseAggregatableReport decodes an aggregatable report body without decrypting
// the payloads.
func ParseAggregatableReport(body []byte) (*AggregatableReport, error) {
	var r AggregatableReport
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("ara: parse aggregatable report: %w", err)
	}
	// attribution_destination and report_id normally live inside the JSON-encoded
	// shared_info on a real aggregatable report; accept them there too.
	if (r.AttributionDestination == "" || r.ReportID == "") && r.SharedInfo != "" {
		var si struct {
			AttributionDestination string `json:"attribution_destination"`
			ReportID               string `json:"report_id"`
		}
		if json.Unmarshal([]byte(r.SharedInfo), &si) == nil {
			if r.AttributionDestination == "" {
				r.AttributionDestination = si.AttributionDestination
			}
			if r.ReportID == "" {
				r.ReportID = si.ReportID
			}
		}
	}
	if r.AttributionDestination == "" {
		return nil, fmt.Errorf("ara: aggregatable report missing attribution_destination")
	}
	return &r, nil
}
