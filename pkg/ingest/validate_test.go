package ingest

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
)

// ValidateSample is the synchronous pre-flight: it needs only a Pipeline, so it
// can be exercised without object storage / Postgres.
func sampleProc() *Processor {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Processor{Pipeline: pipeline.New(log), Log: log}
}

func TestValidateSample(t *testing.T) {
	p := sampleProc()
	spec := ingestjobs.SegmentSpec{} // API upload — defaults (id column mappings)

	cases := []struct {
		name       string
		file       string
		wantReject bool
	}{
		{"lowercase user_id header", "user_id\nu1\nu2\n", false},
		{"UPPERCASE header is case-insensitive", "USER_ID\nu1\n", false},
		{"mixed-case + spaces", "  User_Id \nu1\n", false},
		{"hashed_email column resolves", "hashed_email\nabc123\n", false},
		{"no id column → reject", "email_address,city\nfoo@bar.com,NYC\n", true},
		{"header only, no data → reject", "user_id\n", true},
		{"empty file → reject", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := p.ValidateSample(context.Background(), "upload.csv", []byte(c.file), spec, 10)
			if c.wantReject {
				if err == nil {
					t.Fatalf("expected reject, got nil")
				}
				if !IsReject(err) {
					t.Fatalf("expected a rejectErr, got %T: %v", err, err)
				}
			} else if err != nil {
				t.Fatalf("expected accept, got reject: %v", err)
			}
		})
	}
}
