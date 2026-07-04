package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quietMgmtLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestValidDate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true}, {"2026-07-05", true}, {"2026-13-01", false},
		{"07-05-2026", false}, {"2026/07/05", false}, {"nonsense", false},
	} {
		if got := validDate(tc.in); got != tc.want {
			t.Errorf("validDate(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// handleCreate validates the request BEFORE any DB access, so a nil DB is
// fine for the rejection cases — they must 400 without dialing Postgres.
func TestHandleCreate_ValidationRejections(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"bad bid_strategy", `{"name":"x","bid_strategy":"cpx"}`},
		{"bad pacing_mode", `{"name":"x","pacing_mode":"turbo"}`},
		{"bad start_date", `{"name":"x","start_date":"07-05-2026"}`},
		{"end before start", `{"name":"x","start_date":"2026-07-10","end_date":"2026-07-01"}`},
		{"bad viewability", `{"name":"x","viewability_target_pct":150}`},
		{"missing name", `{"base_bid":2}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/dsp/campaigns", strings.NewReader(tc.body))
			handleCreate(rec, req, nil, nil, "dsp-1", "internal", quietMgmtLog())
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandlePatch_ValidationRejections(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"empty patch", `{}`},
		{"bad status", `{"status":"exploded"}`},
		{"bad bid_strategy", `{"bid_strategy":"cpx"}`},
		{"bad pacing_mode", `{"pacing_mode":"turbo"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/v1/dsp/campaigns/li-1", strings.NewReader(tc.body))
			handlePatch(rec, req, nil, nil, "li-1", quietMgmtLog())
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}
