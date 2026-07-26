//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
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

// CreateAudienceMapping saves a tenant custom field mapping (ADR 0008) and
// returns its id. mappings is their-column → canonical (only id_value/id_type).
func (h *Harness) CreateAudienceMapping(t *testing.T, accountID, name string, mappings map[string]string, idType string) string {
	t.Helper()
	email := "aud-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")
	body, _ := json.Marshal(map[string]any{"name": name, "mappings": mappings, "id_type": idType})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIAudienceMappings, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create mapping status %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &out)
	return out.ID
}

// UploadAudienceCSVMapped uploads a CSV applying a saved mapping_id.
func (h *Harness) UploadAudienceCSVMapped(t *testing.T, accountID, name, visibility, csv, mappingID string) (int, string) {
	t.Helper()
	email := "aud-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("visibility", visibility)
	_ = mw.WriteField("mapping_id", mappingID)
	fw, _ := mw.CreateFormFile("file", "upload.csv")
	_, _ = fw.Write([]byte(csv))
	_ = mw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIAudiences, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("mapped upload: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// IngestHistory GETs the account's upload history (ADR 0007/0008) and returns
// the raw JSON body — the tenant-scoped list of ingest jobs.
func (h *Harness) IngestHistory(t *testing.T, accountID string) string {
	t.Helper()
	email := "aud-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+routes.APIAudienceIngest, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("ingest history: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest history status %d: %s", resp.StatusCode, string(b))
	}
	return string(b)
}

// UploadAudienceCSVNotify uploads a CSV with additional notification emails
// (ADR 0008) and returns the raw (status, body).
func (h *Harness) UploadAudienceCSVNotify(t *testing.T, accountID, name, visibility, csv string, additional []string) (int, string) {
	t.Helper()
	email := "aud-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("visibility", visibility)
	_ = mw.WriteField("additional_emails", strings.Join(additional, ","))
	fw, _ := mw.CreateFormFile("file", "upload.csv")
	_, _ = fw.Write([]byte(csv))
	_ = mw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIAudiences, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("notify upload: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// PGPPublicKey fetches the platform PGP public key (ADR 0008) as an authed
// tenant user — what a provider would grab to encrypt their file.
func (h *Harness) PGPPublicKey(t *testing.T, accountID string) string {
	t.Helper()
	email := "aud-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+routes.APIAudiencePGPKey, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("pgp-key call: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pgp-key status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		PublicKey   string `json:"public_key"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode pgp-key: %v", err)
	}
	return out.PublicKey
}

// UploadAudienceCSVStatus POSTs a CSV upload and returns the raw (status, body)
// without asserting 200 — for the reject-path tests (a wrong-shaped file must
// come back 422 with a reason).
func (h *Harness) UploadAudienceCSVStatus(t *testing.T, accountID, name, visibility, csv string) (int, string) {
	t.Helper()
	email := "aud-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("visibility", visibility)
	fw, _ := mw.CreateFormFile("file", "upload.csv")
	_, _ = fw.Write([]byte(csv))
	_ = mw.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIAudiences, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("csv upload call: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
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

// SignalResidual counts the user's rows in the ClickHouse profile-store tables
// (profile_signals keyed on id_value, behaviour_signals on user_id/household_id)
// — the GDPR residual check, also handy as "have this user's rows landed yet?"
// polling. Since ADR 0006 phase 5 these live in ClickHouse (the single store),
// not the retired Delta lake.
func (h *Harness) SignalResidual(t *testing.T, userID string) map[string]int {
	t.Helper()
	return map[string]int{
		"profile_signals": h.ClickHouseScalar(t, fmt.Sprintf(
			"SELECT count() FROM adtech.profile_signals WHERE id_value='%s'", userID)),
		"behaviour_signals": h.ClickHouseScalar(t, fmt.Sprintf(
			"SELECT count() FROM adtech.behaviour_signals WHERE user_id='%s' OR household_id='%s'", userID, userID)),
	}
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

// UploadAudienceCSVProviderStatus is UploadAudienceCSVStatus with a
// provider_id form field (ADR 0009): the upload snapshots the provider's
// party/licence/id_type defaults and enforces its encryption contract.
// Returns status + body so tests can assert both the accept and the
// encryption_expected cleartext-reject paths.
func (h *Harness) UploadAudienceCSVProviderStatus(t *testing.T, accountID, name, visibility, csv, providerID string) (int, string) {
	t.Helper()
	email := "aud-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	_ = mw.WriteField("visibility", visibility)
	_ = mw.WriteField("provider_id", providerID)
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
		t.Fatalf("build provider csv upload: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("provider csv upload call: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}
