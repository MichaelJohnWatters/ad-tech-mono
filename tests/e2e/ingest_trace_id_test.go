//go:build e2e

// Upload lineage vs request trace. Uploaded audience data is BATCH: no single
// request trace spans it (the async worker may process it later, under no
// request). So its lineage rides a DISTINCT, self-identifying field —
// ingest_trace_id = "ing_<32hex>" derived from the ingest job
// (audience_ingest_jobs.id) — deliberately NOT the same format as a 32-hex
// request trace_id, so the two can never be confused. This proves, on real
// ClickHouse, that every uploaded profile_signal carries an ing_-format
// ingest_trace_id that maps deterministically back to its ingest job, and that
// no uploaded row's trace_id ever carries the ingest format.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestUploadCarriesIngestTraceIDNotTraceID(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "ingest-trace")

	uniq := time.Now().UnixNano()
	idVal := fmt.Sprintf("%s-ingtrace-%d", w.AdvAcc.ID, uniq)
	csv := "user_id\n" + idVal + "\n"

	res := h.UploadAudienceCSV(t, w.AdvAcc.ID, fmt.Sprintf("ingtrace-%d", uniq), "dsp_private", csv)
	if res.SegmentID == "" {
		t.Fatal("upload returned empty segment id")
	}

	countBy := func(where string) int {
		return h.ClickHouseScalar(t, fmt.Sprintf(
			"SELECT count() FROM adtech.profile_signals WHERE id_value='%s'%s", idVal, where))
	}

	// Wait for the uploaded id's profile_signal to land in ClickHouse.
	deadline := time.Now().Add(45 * time.Second)
	for countBy("") < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("profile_signal for %s never landed in ClickHouse", idVal)
		}
		time.Sleep(2 * time.Second)
	}
	total := countBy("")

	// 1) Every uploaded row carries the DISTINCT-format ingest_trace_id
	//    ("ing_" + 32 hex = 36 chars) — the batch lineage is always present.
	if got := countBy(" AND startsWith(ingest_trace_id,'ing_') AND length(ingest_trace_id)=36"); got != total {
		t.Errorf("well-formed ingest_trace_id rows = %d, want %d (all 'ing_<32hex>')", got, total)
	}

	// 2) trace_id is NEVER in the ingest format — the two fields can't be
	//    confused. (Uploaded rows' trace_id is a real 32-hex request trace when
	//    inline, empty when async — never an ing_ value.)
	if got := countBy(" AND startsWith(trace_id,'ing_')"); got != 0 {
		t.Errorf("%d uploaded rows have an ing_-format trace_id — the two fields are confusable", got)
	}

	// 3) ingest_trace_id maps deterministically back to the ingest job that
	//    produced the row (audience_ingest_jobs.id, dash-stripped, ing_-prefixed).
	var jobID string
	if err := h.DB.QueryRow(
		`SELECT id::text FROM audience_ingest_jobs WHERE segment_id = $1::uuid ORDER BY created_at DESC LIMIT 1`,
		res.SegmentID).Scan(&jobID); err != nil {
		t.Fatalf("look up ingest job for segment %s: %v", res.SegmentID, err)
	}
	want := "ing_" + strings.ReplaceAll(jobID, "-", "")
	if got := countBy(fmt.Sprintf(" AND ingest_trace_id='%s'", want)); got != total {
		t.Errorf("rows mapping to job %s = %d, want %d (ingest_trace_id must equal %q)", jobID, got, total, want)
	}
}
