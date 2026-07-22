package ingest

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/datalake"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

func TestDecodeFile(t *testing.T) {
	ctx := context.Background()

	t.Run("csv", func(t *testing.T) {
		recs, err := DecodeFile(ctx, "list.csv", []byte("user_id,geo\nu1,US\nu2,GB\n"))
		if err != nil || len(recs) != 2 {
			t.Fatalf("recs=%d err=%v", len(recs), err)
		}
		if recs[0]["user_id"] != "u1" {
			t.Errorf("first row: %v", recs[0])
		}
	})

	t.Run("tsv by extension", func(t *testing.T) {
		recs, err := DecodeFile(ctx, "list.tsv", []byte("user_id\tgeo\nu1\tUS\n"))
		if err != nil || len(recs) != 1 || recs[0]["geo"] != "US" {
			t.Fatalf("recs=%v err=%v", recs, err)
		}
	})

	t.Run("tsv by sniff when misnamed", func(t *testing.T) {
		recs, err := DecodeFile(ctx, "list.dat", []byte("user_id\tgeo\nu1\tUS\n"))
		if err != nil || len(recs) != 1 || recs[0]["user_id"] != "u1" {
			t.Fatalf("recs=%v err=%v", recs, err)
		}
	})

	t.Run("gzip auto-detect and unwrap", func(t *testing.T) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte("user_id\nu1\nu2\n"))
		_ = zw.Close()
		recs, err := DecodeFile(ctx, "list.csv.gz", buf.Bytes())
		if err != nil || len(recs) != 2 {
			t.Fatalf("recs=%d err=%v", len(recs), err)
		}
	})

	t.Run("zip with multiple part files", func(t *testing.T) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, body := range map[string]string{
			"part-1.csv": "user_id\nu1\n",
			"part-2.tsv": "user_id\tgeo\nu2\tUS\n",
		} {
			w, _ := zw.Create(name)
			_, _ = w.Write([]byte(body))
		}
		_ = zw.Close()
		recs, err := DecodeFile(ctx, "list.zip", buf.Bytes())
		if err != nil || len(recs) != 2 {
			t.Fatalf("recs=%d err=%v", len(recs), err)
		}
	})

	t.Run("parquet by magic", func(t *testing.T) {
		// Real round-trip: encode a parquet file via the datalake writer,
		// pull the raw object bytes back out, decode through the processor.
		obj, err := fs.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		_ = obj.EnsureBucket(ctx, "b")
		lake := datalake.NewObjectStore(obj, "b", slog.New(slog.NewTextHandler(io.Discard, nil)))
		schema := datalake.Schema{Version: 1, Columns: []datalake.Column{
			{Name: "user_id", Type: "string", Nullable: true},
			{Name: "score", Type: "int64", Nullable: true},
		}}
		if err := lake.Write(ctx, "t", []datalake.Record{
			{"user_id": "u1", "score": int64(9)},
			{"user_id": "u2", "score": int64(4)},
		}, schema); err != nil {
			t.Fatal(err)
		}
		keys, _ := obj.List(ctx, "b", "t/")
		var raw []byte
		for _, k := range keys {
			if bytes.HasSuffix([]byte(k), []byte(".parquet")) {
				rc, _ := obj.Get(ctx, "b", k)
				raw, _ = io.ReadAll(rc)
				rc.Close()
			}
		}
		if len(raw) == 0 {
			t.Fatal("no parquet object written")
		}
		recs, err := DecodeFile(ctx, "list.parquet", raw)
		if err != nil || len(recs) != 2 {
			t.Fatalf("recs=%d err=%v", len(recs), err)
		}
		if recs[0]["user_id"] != "u1" || recs[0]["score"] != "9" {
			t.Errorf("typed values not stringified: %v", recs[0])
		}
	})

	t.Run("segment name strips stacked extensions", func(t *testing.T) {
		for in, want := range map[string]string{
			"auto-intenders.csv.gz": "auto-intenders",
			"list.parquet":          "list",
			"seg.zip":               "seg",
			"plain.csv":             "plain",
		} {
			if got := SegmentNameFromFile(in); got != want {
				t.Errorf("SegmentNameFromFile(%q) = %q, want %q", in, got, want)
			}
		}
	})
}
