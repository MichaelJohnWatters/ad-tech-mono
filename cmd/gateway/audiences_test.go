package main

import (
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
)

func parseCSV(t *testing.T, csv string) []pipeline.Record {
	t.Helper()
	recs, err := pipeline.IngestCSV(strings.NewReader(csv), ',')
	if err != nil {
		t.Fatalf("IngestCSV: %v", err)
	}
	return recs
}

func TestExtractMemberIDs(t *testing.T) {
	t.Run("user_id column", func(t *testing.T) {
		ids, errMsg := extractMemberIDs(parseCSV(t, "user_id\nu1\nu2\n\nu3"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if len(ids) != 3 {
			t.Fatalf("want 3 ids, got %d", len(ids))
		}
		if ids[0].IDType != "user_id" || ids[0].IDValue != "u1" {
			t.Fatalf("unexpected first id: %+v", ids[0])
		}
	})

	t.Run("hashed_email column implies id_type", func(t *testing.T) {
		ids, errMsg := extractMemberIDs(parseCSV(t, "hashed_email\nabc123\ndef456"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if ids[0].IDType != "hashed_email" {
			t.Fatalf("want id_type hashed_email, got %q", ids[0].IDType)
		}
	})

	t.Run("explicit id_type column overrides", func(t *testing.T) {
		ids, errMsg := extractMemberIDs(parseCSV(t, "id_value,id_type\nv1,uid2\nv2,"))
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if ids[0].IDType != "uid2" {
			t.Fatalf("want uid2, got %q", ids[0].IDType)
		}
		if ids[1].IDType != "user_id" {
			t.Fatalf("empty id_type should default to user_id, got %q", ids[1].IDType)
		}
	})

	t.Run("raw email rejected", func(t *testing.T) {
		_, errMsg := extractMemberIDs(parseCSV(t, "user_id\nsomeone@example.com"))
		if !strings.Contains(errMsg, "hash PII") {
			t.Fatalf("want raw-email rejection, got %q", errMsg)
		}
	})

	t.Run("no id column", func(t *testing.T) {
		_, errMsg := extractMemberIDs(parseCSV(t, "name,age\nbob,4"))
		if !strings.Contains(errMsg, "no id column") {
			t.Fatalf("want no-id-column error, got %q", errMsg)
		}
	})

	t.Run("empty rows only", func(t *testing.T) {
		_, errMsg := extractMemberIDs(parseCSV(t, "user_id\n\n"))
		if errMsg == "" {
			t.Fatal("want error for empty ids")
		}
	})
}
