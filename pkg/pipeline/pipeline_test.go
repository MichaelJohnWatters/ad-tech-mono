package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
)

func TestPipeline_Process(t *testing.T) {
	log := logger.New("pipeline-test")
	p := New(log)

	records := []Record{
		{"campaign": "c1", "impressions": "100", "geo": "gbr", "device": "Mobile"},
		{"campaign": "c2", "impressions": "200", "geo": "usa", "device": "Desktop"},
		{"campaign": "", "impressions": "50", "geo": "fra"}, // missing required
	}

	cfg := PublisherConfig{
		PublisherID:    "test-pub",
		RequiredFields: []string{"campaign_id", "impressions"},
		FieldMappings:  map[string]string{"campaign": "campaign_id"},
	}

	result := p.Process(context.Background(), records, cfg)

	if result.Stats.TotalInput != 3 {
		t.Errorf("total = %d, want 3", result.Stats.TotalInput)
	}
	if result.Stats.Valid != 2 {
		t.Errorf("valid = %d, want 2", result.Stats.Valid)
	}
	if result.Stats.Quarantined != 1 {
		t.Errorf("quarantined = %d, want 1", result.Stats.Quarantined)
	}

	// Check normalisation: campaign -> campaign_id
	if result.Valid[0]["campaign_id"] != "c1" {
		t.Errorf("expected campaign_id=c1, got %s", result.Valid[0]["campaign_id"])
	}

	// Check enrichment: geo uppercased
	if result.Valid[0]["geo"] != "GBR" {
		t.Errorf("expected geo=GBR, got %s", result.Valid[0]["geo"])
	}

	// Check enrichment: device lowercased
	if result.Valid[0]["device"] != "mobile" {
		t.Errorf("expected device=mobile, got %s", result.Valid[0]["device"])
	}

	// Check quarantine error
	if len(result.Quarantine) != 1 {
		t.Fatal("expected 1 quarantined record")
	}
	qr := result.Quarantine[0]
	if len(qr.Errors) == 0 {
		t.Error("expected validation errors on quarantined record")
	}
}

func TestIngestCSV(t *testing.T) {
	input := `Campaign,Impressions,Geo
c1,100,GBR
c2,200,USA
c3,300,FRA
`
	records, err := IngestCSV(strings.NewReader(input), ',')
	if err != nil {
		t.Fatal(err)
	}

	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(records))
	}

	// Headers should be lowercased
	if records[0]["campaign"] != "c1" {
		t.Errorf("expected campaign=c1, got %s", records[0]["campaign"])
	}
	if records[0]["impressions"] != "100" {
		t.Errorf("expected impressions=100, got %s", records[0]["impressions"])
	}
}

func TestIngestCSV_TSV(t *testing.T) {
	input := "name\tvalue\na\t1\nb\t2\n"
	records, err := IngestCSV(strings.NewReader(input), '\t')
	if err != nil {
		t.Fatal(err)
	}

	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
}

func TestValidationError(t *testing.T) {
	err := ValidationError{
		Field:   "campaign",
		Value:   "",
		Rule:    "required",
		Message: "field is required but missing or empty",
	}
	s := err.Error()
	if !strings.Contains(s, "campaign") {
		t.Errorf("error string missing field name: %s", s)
	}
}
