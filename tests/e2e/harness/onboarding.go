//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	neturl "net/url"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
	objs3 "github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/s3"
)

// AudienceUploadResult mirrors the gateway's upload response.
type AudienceUploadResult struct {
	SegmentID    string  `json:"segment_id"`
	MembersAdded int     `json:"members_added"`
	MembersSent  int     `json:"members_sent"`
	Matched      int     `json:"matched"`
	MatchRate    float64 `json:"match_rate"`
}

// UploadAudienceCSV POSTs the multipart CSV variant of the audience upload —
// the portal's small-file path — authenticated as an owner of accountID.
func (h *Harness) UploadAudienceCSV(t *testing.T, accountID, name, visibility, csv string) AudienceUploadResult {
	t.Helper()
	email := "aud-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("visibility", visibility)
	fw, err := mw.CreateFormFile("file", "upload.csv")
	if err != nil {
		t.Fatalf("multipart file: %v", err)
	}
	if _, err := fw.Write([]byte(csv)); err != nil {
		t.Fatalf("multipart write: %v", err)
	}
	_ = mw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIAudiences, &buf)
	if err != nil {
		t.Fatalf("build csv upload: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("csv upload call: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("csv upload status %d: %s", resp.StatusCode, string(body))
	}
	var out AudienceUploadResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode csv upload response: %v", err)
	}
	return out
}

// OnboardingBucket returns an S3 client on the local Minio plus the
// drop-zone bucket name, for landing provider files the pipeline poller
// ingests. Credentials match k8s/base (same convention as
// routes.DefaultPostgresURL embedding the local-dev password).
func (h *Harness) OnboardingBucket(t *testing.T) (objects.Store, string) {
	t.Helper()
	store, err := objs3.New(objs3.Config{
		Endpoint:  h.URLs.MinioEndpt,
		AccessKey: "adtech",
		SecretKey: "adtech-local-dev",
		Region:    "us-east-1",
		UseSSL:    false,
	})
	if err != nil {
		t.Fatalf("minio connect: %v", err)
	}
	const bucket = "adtech-onboarding"
	if err := store.EnsureBucket(context.Background(), bucket); err != nil {
		t.Fatalf("ensure onboarding bucket: %v", err)
	}
	return store, bucket
}

// LakeResidual returns the pipeline's per-table counts of lake rows keyed to
// userID (profile_signals / behaviour_signals) — the GDPR residual check,
// also handy as "have this user's rows landed yet?" polling.
func (h *Harness) LakeResidual(t *testing.T, userID string) map[string]int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u := routes.DefaultPipelineURL + routes.DatalakeResidual + "?user_id=" + neturl.QueryEscape(userID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("lake residual: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lake residual status %d: %s", resp.StatusCode, string(body))
	}
	var counts map[string]int
	if err := json.Unmarshal(body, &counts); err != nil {
		t.Fatalf("decode lake residual: %v", err)
	}
	return counts
}

// SeedIdentityEdges inserts identity_graph edges for the given ids (each
// linked to a synthetic hashed-email), making them "known" to the graph so
// the onboarding match rate has a numerator.
func (h *Harness) SeedIdentityEdges(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := h.DB.Exec(`
INSERT INTO identity_graph (user_id, linked_id, source, link_type, confidence)
VALUES ($1, $2, 'hashed_email', 'crm_match', 1.0)
ON CONFLICT (user_id, linked_id, source) DO NOTHING`, id, "em:"+id); err != nil {
			t.Fatalf("seed identity edge for %s: %v", id, err)
		}
	}
}
