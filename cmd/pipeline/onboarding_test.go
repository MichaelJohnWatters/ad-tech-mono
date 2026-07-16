package main

import (
	"context"
	"encoding/csv"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

func TestRenderRejectedCSV(t *testing.T) {
	rows := []pipeline.QuarantineRecord{
		{
			Record: pipeline.Record{"id_value": "", "geo": "US"},
			Errors: []pipeline.ValidationError{{Field: "id_value", Rule: "required", Message: "field is required but missing or empty"}},
		},
	}
	body, err := renderRejectedCSV(rows)
	if err != nil {
		t.Fatalf("renderRejectedCSV: %v", err)
	}
	recs, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want header + 1 row, got %d", len(recs))
	}
	header := strings.Join(recs[0], ",")
	if header != "geo,id_value,_errors" {
		t.Fatalf("unexpected header order: %s", header)
	}
	if !strings.Contains(recs[1][2], "required") {
		t.Fatalf("errors column missing rule: %q", recs[1][2])
	}
}

// TestDeleteArtifacts — the retention sweep's object-deletion half removes
// every copy a run leaves behind and tolerates the quarantined-whole-file
// case (no processed/ copy).
func TestDeleteArtifacts(t *testing.T) {
	obj, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	ctx := context.Background()
	const bucket = "adtech-onboarding"
	_ = obj.EnsureBucket(ctx, bucket)
	put := func(key string) {
		if err := obj.Put(ctx, bucket, key, strings.NewReader("x"), 1, "text/csv"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	put("acme/processed/list.csv.gz")
	put("acme/rejected/list.csv.gz")
	put("acme/rejected/list.csv.gz.error.txt")

	o := &onboarder{obj: obj, bucket: bucket, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := o.deleteArtifacts(ctx, "acme", "acme/incoming/list.csv.gz", "acme/rejected/list.csv.gz"); err != nil {
		t.Fatalf("deleteArtifacts: %v", err)
	}
	for _, key := range []string{"acme/processed/list.csv.gz", "acme/rejected/list.csv.gz", "acme/rejected/list.csv.gz.error.txt"} {
		if ok, _ := obj.Exists(ctx, bucket, key); ok {
			t.Errorf("%s survived the sweep", key)
		}
	}
	// Quarantine-only run (no processed copy, no rejected key) is a no-op.
	if err := o.deleteArtifacts(ctx, "acme", "acme/incoming/ghost.csv", ""); err != nil {
		t.Errorf("no-op sweep errored: %v", err)
	}
}

// TestOnboarderQuarantinesFileWithoutManifest exercises the poller's content-
// failure path against a real (filesystem) object store: a CSV landing in a
// provider zone with no manifest.json moves to rejected/ with an error
// marker, and incoming/ drains so the next tick doesn't reprocess.
func TestOnboarderQuarantinesFileWithoutManifest(t *testing.T) {
	obj, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatalf("fs.New: %v", err)
	}
	ctx := context.Background()
	const bucket = "adtech-onboarding"
	if err := obj.EnsureBucket(ctx, bucket); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	put := func(key, body string) {
		t.Helper()
		if err := obj.Put(ctx, bucket, key, strings.NewReader(body), int64(len(body)), "text/csv"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	put("acme/incoming/list.csv", "user_id\nu1\n")

	o := &onboarder{
		obj: obj, bucket: bucket,
		pipe: pipeline.New(slog.New(slog.NewTextHandler(io.Discard, nil))),
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	o.tick(ctx)

	if ok, _ := obj.Exists(ctx, bucket, "acme/incoming/list.csv"); ok {
		t.Fatal("incoming file should have been moved")
	}
	if ok, _ := obj.Exists(ctx, bucket, "acme/rejected/list.csv"); !ok {
		t.Fatal("rejected copy missing")
	}
	if ok, _ := obj.Exists(ctx, bucket, "acme/rejected/list.csv.error.txt"); !ok {
		t.Fatal("error marker missing")
	}
}
