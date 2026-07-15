package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	audiencepg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/audience/store/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/pipeline"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/tracing"
)

// audienceUploadRequest is a CRM/audience upload: create-or-find a named
// segment for an account and bulk-add user memberships. This is the
// production write path the audience pipeline (CRM import, behavioural
// rollup, lookalike publish) uses — previously memberships only ever
// arrived via seed/e2e raw SQL.
//
// account_id is IGNORED — the handler binds the segment to the authenticated
// account from the JWT claims, so a caller can never write another tenant's
// data. The field remains for backward-compatible request bodies.
type audienceUploadRequest struct {
	AccountID  string   `json:"account_id"`
	Name       string   `json:"name"`
	Type       string   `json:"type,omitempty"`       // default first_party
	Visibility string   `json:"visibility,omitempty"` // public | dsp_private (default dsp_private)
	Source     string   `json:"source,omitempty"`     // default crm_upload
	UserIDs    []string `json:"user_ids"`
}

type audienceUploadResponse struct {
	SegmentID    string  `json:"segment_id"`
	MembersAdded int     `json:"members_added"`
	MembersSent  int     `json:"members_sent"`
	Matched      int     `json:"matched"`
	MatchRate    float64 `json:"match_rate"`
}

// maxAudienceUploadBytes caps the multipart CSV variant. Larger third-party
// files go via the Minio drop-zone (adtech-onboarding bucket) which the
// pipeline polls — the portal upload is the small-file convenience path.
const maxAudienceUploadBytes = 5 << 20

// maxAudienceUploadRows bounds a single upload's row count so the synchronous
// handler (member insert + match-rate query + signal publish) stays
// interactive. Bigger lists belong in the drop-zone.
const maxAudienceUploadRows = 50000

// profileSignalChunk is how many ids ride in one ProfileSignalEvent message —
// sized well under the NATS 1MB payload cap.
const profileSignalChunk = 1000

// identityMatcher answers "how many of these ids does the identity graph
// know?" — the match-rate numerator. Implemented by pkg/store/postgres.Store.
type identityMatcher interface {
	CountKnownIdentifiers(ctx context.Context, ids []string) (int, error)
}

// audienceHandler serves the tenant-scoped audiences API for the customer
// portal:
//
//	GET  /v1/api/audiences — list the account's segments (with member counts + match rate)
//	POST /v1/api/audiences — upload a named segment + bulk-add members
//	  - application/json: {name, type, visibility, user_ids}
//	  - multipart/form-data: fields name/type/visibility + a CSV file (≤5MB).
//	    PII must be hashed client-side before transmission (the portal does).
//
// Both bind to the authenticated account from the JWT claims; a body
// account_id is ignored, so a caller can never read or write another tenant's
// data. Every upload computes + persists the segment's match rate (fraction of
// ids known to identity_graph), publishes the normalized rows to the
// profile_signals lake table via NATS, and publishes an audience
// cache-invalidate so the DSP/SSP warm caches pick the new members up within a
// round-trip.
func audienceHandler(store *audiencepg.Store, matcher identityMatcher, bus events.EventBus, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if devTenantGuard(w, r, claims, []audiencepg.Segment{}) {
			return
		}
		if store == nil {
			http.Error(w, `{"error":"audience store unavailable"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.Method {
		case http.MethodGet:
			if !can(claims, "audiences:read") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			segs, err := store.ListSegments(r.Context(), claims.AccountID)
			if err != nil {
				log.Error("audience list failed", "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(segs)

		case http.MethodPost:
			if !can(claims, "audiences:upload") {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			var req audienceUploadRequest
			var ids []events.ProfileSignalID
			var src string

			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mediaType == "multipart/form-data" {
				var errMsg string
				req, ids, errMsg = parseMultipartUpload(w, r)
				if errMsg != "" {
					http.Error(w, `{"error":`+jsonStr(errMsg)+`}`, http.StatusBadRequest)
					return
				}
				src = "portal_csv"
			} else {
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
					return
				}
				for _, uid := range req.UserIDs {
					if uid = strings.TrimSpace(uid); uid != "" {
						ids = append(ids, events.ProfileSignalID{IDType: "user_id", IDValue: uid})
					}
				}
				src = "crm_upload"
			}

			// Bind to the authenticated tenant — never trust a body account_id.
			req.AccountID = claims.AccountID
			if req.Name == "" {
				http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
				return
			}
			if len(ids) == 0 {
				http.Error(w, `{"error":"no user ids in upload"}`, http.StatusBadRequest)
				return
			}
			if len(ids) > maxAudienceUploadRows {
				http.Error(w, fmt.Sprintf(`{"error":"upload exceeds %d rows — use the partner drop-zone for large files"}`, maxAudienceUploadRows), http.StatusBadRequest)
				return
			}
			if req.Type == "" {
				req.Type = "first_party"
			}
			if !isValidSegmentType(req.Type) {
				http.Error(w, `{"error":"invalid type"}`, http.StatusBadRequest)
				return
			}
			if req.Visibility == "" {
				req.Visibility = "dsp_private"
			}
			if req.Visibility != "public" && req.Visibility != "dsp_private" {
				http.Error(w, `{"error":"visibility must be public or dsp_private"}`, http.StatusBadRequest)
				return
			}
			if req.Source == "" {
				req.Source = src
			}

			resp, err := runAudienceUpload(r.Context(), store, matcher, bus, log, req, ids)
			if err != nil {
				log.Error("audience upload failed", "name", req.Name, "error", err)
				http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			log.Info("audience upload", "segment", resp.SegmentID, "name", req.Name,
				"added", resp.MembersAdded, "sent", resp.MembersSent,
				"matched", resp.Matched, "match_rate", resp.MatchRate)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(resp)

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// runAudienceUpload is the shared upload core for the JSON and multipart
// variants: upsert segment, add members, compute + persist match rate,
// publish profile signals to the lake, invalidate warm caches.
func runAudienceUpload(ctx context.Context, store *audiencepg.Store, matcher identityMatcher,
	bus events.EventBus, log *slog.Logger, req audienceUploadRequest, ids []events.ProfileSignalID,
) (audienceUploadResponse, error) {
	segmentID, err := store.UpsertSegment(ctx, req.AccountID, req.Name, req.Type, req.Source, req.Visibility)
	if err != nil {
		return audienceUploadResponse{}, fmt.Errorf("upsert segment: %w", err)
	}
	values := make([]string, len(ids))
	for i, id := range ids {
		values[i] = id.IDValue
	}
	added, err := store.AddMembers(ctx, req.AccountID, segmentID, values)
	if err != nil {
		return audienceUploadResponse{}, fmt.Errorf("add members: %w", err)
	}

	// Match rate: fraction of uploaded ids the identity graph can resolve.
	// Advisory (upload feedback), so a failure degrades to 0 with an ERROR
	// log rather than failing the whole upload.
	matched := 0
	if matcher != nil {
		if matched, err = matcher.CountKnownIdentifiers(ctx, values); err != nil {
			log.Error("audience upload: match-rate query failed", "segment", segmentID, "error", err)
			matched = 0
		} else if err := store.SetSegmentUploadStats(ctx, req.AccountID, segmentID, len(values), matched); err != nil {
			log.Error("audience upload: persist match rate failed", "segment", segmentID, "error", err)
		}
	}

	if bus != nil {
		publishProfileSignals(ctx, bus, log, req, segmentID, ids)
		payload := []byte(`{"segment_id":"` + segmentID + `","account_id":"` + req.AccountID + `"}`)
		if err := bus.Publish(ctx, events.SubjectCacheInvalidateAudience, payload); err != nil {
			log.Warn("audience upload: invalidate publish failed", "segment", segmentID, "error", err)
		}
	}

	return audienceUploadResponse{
		SegmentID:    segmentID,
		MembersAdded: added,
		MembersSent:  len(ids),
		Matched:      matched,
		MatchRate:    float64(matched) / float64(len(ids)),
	}, nil
}

// publishProfileSignals emits the upload's normalized rows to the
// profile_signals lake table, chunked so each NATS message stays well under
// the payload cap. Best-effort: the lake copy is the replayable record, and
// the profile-builder's reconcile pass replays PG memberships the lake
// missed, so a failed publish is an ERROR log, not a failed upload.
func publishProfileSignals(ctx context.Context, bus events.EventBus, log *slog.Logger,
	req audienceUploadRequest, segmentID string, ids []events.ProfileSignalID,
) {
	pub := events.NewPublisher(bus, log)
	now := time.Now().UTC()
	for start := 0; start < len(ids); start += profileSignalChunk {
		end := min(start+profileSignalChunk, len(ids))
		ev := events.ProfileSignalEvent{
			SchemaVersion: events.CurrentSchemaVersion,
			TraceID:       tracing.TraceIDFromContext(ctx),
			AccountID:     req.AccountID,
			Source:        req.Source,
			Access:        "first_party",
			SegmentID:     segmentID,
			SegmentName:   req.Name,
			Visibility:    req.Visibility,
			Consent:       true, // uploader-declared basis: first-party CRM data
			ObservedAt:    now,
			IDs:           ids[start:end],
		}
		if err := pub.PublishJSON(ctx, events.SubjectProfileSignal, ev); err != nil {
			log.Error("audience upload: profile signal publish failed",
				"segment", segmentID, "chunk_start", start, "error", err)
			return
		}
	}
}

// parseMultipartUpload handles the CSV file variant. Returns the metadata
// fields, the extracted ids, and a non-empty error message on caller error.
func parseMultipartUpload(w http.ResponseWriter, r *http.Request) (audienceUploadRequest, []events.ProfileSignalID, string) {
	var req audienceUploadRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxAudienceUploadBytes)
	if err := r.ParseMultipartForm(maxAudienceUploadBytes); err != nil {
		return req, nil, "file too large or malformed multipart body (max 5MB — use the partner drop-zone for larger files)"
	}
	req.Name = strings.TrimSpace(r.FormValue("name"))
	req.Type = r.FormValue("type")
	req.Visibility = r.FormValue("visibility")
	file, _, err := r.FormFile("file")
	if err != nil {
		return req, nil, "missing file field"
	}
	defer file.Close()
	records, err := pipeline.IngestCSV(file, ',')
	if err != nil {
		return req, nil, "invalid CSV: " + err.Error()
	}
	ids, errMsg := extractMemberIDs(records)
	return req, ids, errMsg
}

// idColumns maps recognised CSV header names to the id_type stamped on the
// profile signal. Ordered by preference — id_value/user_id first so a file
// with several id columns picks the canonical one.
var idColumns = []struct{ column, idType string }{
	{"id_value", ""}, // id_type from the id_type column, else user_id
	{"user_id", "user_id"},
	{"id", "user_id"},
	{"hashed_email", "hashed_email"},
	{"email_sha256", "hashed_email"},
	{"uid2", "uid2"},
	{"device_id", "device_id"},
}

// extractMemberIDs pulls (id_type, id_value) pairs out of parsed CSV records.
// The id column is the first recognised header; an explicit id_type column
// overrides the column-implied type per row. Raw emails are rejected — PII
// must be hashed client-side before transmission.
func extractMemberIDs(records []pipeline.Record) ([]events.ProfileSignalID, string) {
	if len(records) == 0 {
		return nil, "CSV has no data rows"
	}
	column, implied := "", ""
	for _, c := range idColumns {
		if _, ok := records[0][c.column]; ok {
			column, implied = c.column, c.idType
			break
		}
	}
	if column == "" {
		return nil, "no id column found (expected one of: id_value, user_id, id, hashed_email, uid2, device_id)"
	}
	var ids []events.ProfileSignalID
	for _, rec := range records {
		v := strings.TrimSpace(rec[column])
		if v == "" {
			continue
		}
		if strings.Contains(v, "@") {
			return nil, "raw email addresses detected — hash PII client-side before upload"
		}
		t := implied
		if explicit := strings.TrimSpace(rec["id_type"]); explicit != "" {
			t = explicit
		}
		if t == "" {
			t = "user_id"
		}
		ids = append(ids, events.ProfileSignalID{IDType: t, IDValue: v})
	}
	if len(ids) == 0 {
		return nil, "CSV has no non-empty ids"
	}
	return ids, ""
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func isValidSegmentType(t string) bool {
	switch t {
	case "first_party", "behavioral", "lookalike", "suppression",
		"retargeting", "composite", "predictive", "cdp_imported":
		return true
	}
	return false
}
