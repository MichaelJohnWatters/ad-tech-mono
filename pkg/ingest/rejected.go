package ingest

import (
	"bytes"
	"encoding/csv"
	"sort"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
)

// renderRejectedCSV serializes quarantined rows with a stable column order
// (sorted union of keys) plus a trailing _errors column.
func renderRejectedCSV(rows []pipeline.QuarantineRecord) ([]byte, error) {
	colSet := map[string]bool{}
	for _, r := range rows {
		for k := range r.Record {
			colSet[k] = true
		}
	}
	cols := make([]string, 0, len(colSet))
	for k := range colSet {
		cols = append(cols, k)
	}
	sort.Strings(cols)

	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(append(append([]string{}, cols...), "_errors")); err != nil {
		return nil, err
	}
	for _, r := range rows {
		row := make([]string, 0, len(cols)+1)
		for _, c := range cols {
			row = append(row, r.Record[c])
		}
		msgs := make([]string, len(r.Errors))
		for i, e := range r.Errors {
			msgs[i] = e.Error()
		}
		row = append(row, strings.Join(msgs, "; "))
		if err := w.Write(row); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}
