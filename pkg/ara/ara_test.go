package ara

import (
	"encoding/json"
	"testing"
	"time"
)

// decode re-parses a marshalled header value so assertions read fields back as
// the browser would, rather than string-matching JSON.
func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("header is not valid JSON: %v\n%s", err, s)
	}
	return m
}

func TestSourceHeader_RequiredAndStringNumerics(t *testing.T) {
	if _, err := (Source{}).MarshalHeader(); err == nil {
		t.Fatal("a source with no destination must error")
	}

	h, err := Source{
		SourceEventID:   18446744073709551615, // max uint64 — must survive as a string
		Destination:     "https://advertiser.example",
		Priority:        100,
		AggregationKeys: map[string]string{"campaignCounts": "0x159"},
		FilterData:      map[string][]string{"product": {"electronics"}},
	}.MarshalHeader()
	if err != nil {
		t.Fatalf("MarshalHeader: %v", err)
	}
	m := decode(t, h)

	// ARA requires the 64-bit numerics to be JSON strings (no float precision loss).
	if got, ok := m["source_event_id"].(string); !ok || got != "18446744073709551615" {
		t.Errorf("source_event_id = %v (%T), want the exact uint64 as a string", m["source_event_id"], m["source_event_id"])
	}
	if _, ok := m["priority"].(string); !ok {
		t.Errorf("priority must be a JSON string, got %T", m["priority"])
	}
	if m["destination"] != "https://advertiser.example" {
		t.Errorf("destination = %v", m["destination"])
	}
	// expiry defaults to 30 days (in seconds, as a string).
	if m["expiry"] != "2592000" {
		t.Errorf("default expiry = %v, want \"2592000\" (30d)", m["expiry"])
	}
}

func TestSourceHeader_ExpiryClamped(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"below min → 1 day", time.Hour, "86400"},
		{"above max → 30 days", 60 * 24 * time.Hour, "2592000"},
		{"in range → exact", 7 * 24 * time.Hour, "604800"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, err := Source{Destination: "https://a.example", Expiry: c.in}.MarshalHeader()
			if err != nil {
				t.Fatal(err)
			}
			if got := decode(t, h)["expiry"]; got != c.want {
				t.Errorf("expiry = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSourceHeader_OmitsEmptyOptionals(t *testing.T) {
	h, _ := Source{SourceEventID: 1, Destination: "https://a.example"}.MarshalHeader()
	m := decode(t, h)
	for _, k := range []string{"priority", "aggregation_keys", "filter_data", "debug_key"} {
		if _, present := m[k]; present {
			t.Errorf("empty %q should be omitted from the header", k)
		}
	}
}

func TestTriggerHeader(t *testing.T) {
	h, err := Trigger{
		EventTriggerData: []EventTriggerData{{TriggerData: 3, Priority: 10, DeduplicationKey: 42}},
		AggregatableTriggerData: []AggregatableTriggerData{
			{KeyPiece: "0x400", SourceKeys: []string{"campaignCounts"}},
		},
		AggregatableValues: map[string]int{"campaignCounts": 32768},
		Filters:            map[string][]string{"product": {"electronics"}},
	}.MarshalHeader()
	if err != nil {
		t.Fatalf("MarshalHeader: %v", err)
	}
	m := decode(t, h)

	etd := m["event_trigger_data"].([]any)[0].(map[string]any)
	if etd["trigger_data"] != "3" { // string per ARA
		t.Errorf("trigger_data = %v (%T), want \"3\"", etd["trigger_data"], etd["trigger_data"])
	}
	if etd["deduplication_key"] != "42" {
		t.Errorf("deduplication_key = %v, want \"42\"", etd["deduplication_key"])
	}
	// aggregatable_values are NUMBERS, not strings.
	av := m["aggregatable_values"].(map[string]any)
	if av["campaignCounts"].(float64) != 32768 {
		t.Errorf("aggregatable_values.campaignCounts = %v, want 32768 (number)", av["campaignCounts"])
	}

	// A trigger with no event or aggregatable data is an error.
	if _, err := (Trigger{}).MarshalHeader(); err == nil {
		t.Error("empty trigger must error")
	}
}

func TestParseEventReport(t *testing.T) {
	body := []byte(`{
		"attribution_destination": "https://advertiser.example",
		"source_event_id": "123456789",
		"trigger_data": "1",
		"source_type": "navigation",
		"report_id": "abc",
		"randomized_trigger_rate": 0.0024
	}`)
	r, err := ParseEventReport(body)
	if err != nil {
		t.Fatalf("ParseEventReport: %v", err)
	}
	if r.SourceEventID != "123456789" || r.TriggerData != "1" || r.SourceType != "navigation" {
		t.Errorf("parsed = %+v", r)
	}
	if _, err := ParseEventReport([]byte(`{"trigger_data":"1"}`)); err == nil {
		t.Error("a report with no attribution_destination must error")
	}
}

func TestParseAggregatableReport_KeepsPayloadsEncrypted(t *testing.T) {
	body := []byte(`{
		"attribution_destination": "https://advertiser.example",
		"source_registration_time": "1700000000",
		"shared_info": "{\"api\":\"attribution-reporting\"}",
		"aggregation_service_payloads": [{"payload":"BASE64CIPHERTEXT","key_id":"key-1"}]
	}`)
	r, err := ParseAggregatableReport(body)
	if err != nil {
		t.Fatalf("ParseAggregatableReport: %v", err)
	}
	if len(r.AggregationServicePayloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(r.AggregationServicePayloads))
	}
	// The payload is kept raw/encrypted — we never decrypt (aggregation service
	// is the mock boundary).
	if !json.Valid(r.AggregationServicePayloads[0]) {
		t.Error("payload envelope should be preserved as raw JSON")
	}
}

func TestParseAggregatableReport_DestAndReportIDFromSharedInfo(t *testing.T) {
	// A real aggregatable report carries attribution_destination + report_id inside
	// the JSON-encoded shared_info, not top-level. We must still recover both.
	body := []byte(`{
		"shared_info": "{\"attribution_destination\":\"https://advertiser.example\",\"report_id\":\"rid-9\"}",
		"aggregation_service_payloads": [{"payload":"CIPHER"}]
	}`)
	r, err := ParseAggregatableReport(body)
	if err != nil {
		t.Fatalf("ParseAggregatableReport: %v", err)
	}
	if r.AttributionDestination != "https://advertiser.example" {
		t.Errorf("destination = %q, want it recovered from shared_info", r.AttributionDestination)
	}
	if r.ReportID != "rid-9" {
		t.Errorf("report_id = %q, want rid-9 (from shared_info)", r.ReportID)
	}
}

func TestNormalizeDestination(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"https://shop.acme.co.uk/checkout?x=1", "https://acme.co.uk", true}, // eTLD+1, path/query stripped
		{"https://www.example.com", "https://example.com", true},
		{"acme.com/x", "https://acme.com", true},                // scheme-less → https
		{"http://sub.example.org", "http://example.org", true},  // scheme preserved
		{"https://localhost:9200/y", "https://localhost", true}, // no public suffix → host (dev), port dropped
		{"", "", false},
		{"https:///nohost", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeDestination(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("NormalizeDestination(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}
