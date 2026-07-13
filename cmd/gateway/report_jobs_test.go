package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/auth"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/reportjobs"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects/fs"
)

const (
	rjPath = "/v1/api/reports/jobs"
	rjAccA = "aaaaaaa1-1111-4111-8111-111111111111"
	rjAccB = "bbbbbbb2-2222-4222-8222-222222222222"
)

// fakeReportJobStore = the real in-memory queue + the two gateway lookups.
type fakeReportJobStore struct {
	*reportjobs.MemoryJobStore
	savedName   string
	savedConfig string
	savedFormat string
	ownerEmail  string
}

func (f *fakeReportJobStore) SavedReportForJob(_ context.Context, accountID, id string) (string, []byte, string, error) {
	if f.savedName == "" {
		return "", nil, "", sql.ErrNoRows
	}
	return f.savedName, []byte(f.savedConfig), f.savedFormat, nil
}
func (f *fakeReportJobStore) OwnerEmail(context.Context, string) (string, error) {
	return f.ownerEmail, nil
}

type fakeJobScope struct{ types map[string]string }

func (f fakeJobScope) AccountType(_ context.Context, id string) (string, error) {
	if t, ok := f.types[id]; ok {
		return t, nil
	}
	return "advertiser", nil
}
func (f fakeJobScope) PublisherIDs(context.Context, string) ([]string, error) {
	return []string{"pub-1"}, nil
}

func newFakeReportJobStore() *fakeReportJobStore {
	return &fakeReportJobStore{MemoryJobStore: reportjobs.NewMemoryJobStore(), ownerEmail: "owner@x.test"}
}

func rjClaims(account string, perms ...string) *auth.Claims {
	return &auth.Claims{AccountID: account, AccountType: auth.AccountAdvertiser,
		UserID: "ddddddd4-4444-4444-8444-444444444444", Permissions: perms}
}

func rjReq(method, target, body string, claims *auth.Claims) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if claims != nil {
		req = withClaims(req, claims)
	}
	return req
}

func TestReportJobsSubmit(t *testing.T) {
	scope := fakeJobScope{}
	adv := rjClaims(rjAccA, "reports:read", "reports:export")

	t.Run("adhoc_submit_scopes_filters", func(t *testing.T) {
		store := newFakeReportJobStore()
		rec := httptest.NewRecorder()
		reportJobsHandler(store, scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
			`{"name":"spend","query_config":{"table":"impressions","metrics":["count"],"filters":{"account_id":"evil"}},"format":"json"}`, adv))
		if rec.Code != http.StatusCreated {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		jobs, _ := store.ListByAccount(context.Background(), rjAccA, 10)
		if len(jobs) != 1 {
			t.Fatalf("jobs=%d, want 1", len(jobs))
		}
		j := jobs[0]
		if j.QueryConfig.Filters["account_id"] != rjAccA {
			t.Errorf("filter account_id=%q, want forced to %s", j.QueryConfig.Filters["account_id"], rjAccA)
		}
		if j.Format != "json" || j.Source != reportjobs.SourceManual || j.Delivery != reportjobs.DeliveryNone {
			t.Errorf("job %+v", j)
		}
		if j.RequestedBy != adv.UserID {
			t.Errorf("requested_by=%q", j.RequestedBy)
		}
	})

	t.Run("template_submit_uses_saved_report", func(t *testing.T) {
		store := newFakeReportJobStore()
		store.savedName, store.savedConfig, store.savedFormat = "Saved weekly", `{"table":"clicks"}`, "parquet"
		rec := httptest.NewRecorder()
		reportJobsHandler(store, scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
			`{"saved_report_id":"ccccccc3-3333-4333-8333-333333333333"}`, adv))
		if rec.Code != http.StatusCreated {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		jobs, _ := store.ListByAccount(context.Background(), rjAccA, 10)
		if jobs[0].Name != "Saved weekly" || jobs[0].Format != "parquet" || jobs[0].QueryConfig.Table != "clicks" {
			t.Errorf("job from template: %+v", jobs[0])
		}
	})

	t.Run("template_not_found_400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobsHandler(newFakeReportJobStore(), scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
			`{"saved_report_id":"ccccccc3-3333-4333-8333-333333333333"}`, adv))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "saved report not found") {
			t.Errorf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("email_delivery_resolves_recipient", func(t *testing.T) {
		store := newFakeReportJobStore()
		rec := httptest.NewRecorder()
		reportJobsHandler(store, scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
			`{"name":"x","query_config":{"table":"impressions"},"delivery":"email"}`, adv))
		if rec.Code != http.StatusCreated {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		jobs, _ := store.ListByAccount(context.Background(), rjAccA, 10)
		if jobs[0].Recipient != "owner@x.test" {
			t.Errorf("recipient=%q", jobs[0].Recipient)
		}
	})

	t.Run("bad_format_400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobsHandler(newFakeReportJobStore(), scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
			`{"name":"x","query_config":{"table":"impressions"},"format":"xlsx"}`, adv))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("code=%d", rec.Code)
		}
	})

	t.Run("missing_table_400", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobsHandler(newFakeReportJobStore(), scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
			`{"name":"x","query_config":{}}`, adv))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("code=%d", rec.Code)
		}
	})

	t.Run("submit_needs_export_perm", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobsHandler(newFakeReportJobStore(), scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
			`{"name":"x","query_config":{"table":"impressions"}}`, rjClaims(rjAccA, "reports:read", "reports:save")))
		if rec.Code != http.StatusForbidden {
			t.Errorf("code=%d, want 403 (reports:save is not reports:export)", rec.Code)
		}
	})

	t.Run("publisher_scope_injected", func(t *testing.T) {
		store := newFakeReportJobStore()
		pubScope := fakeJobScope{types: map[string]string{rjAccA: "publisher"}}
		rec := httptest.NewRecorder()
		reportJobsHandler(store, pubScope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
			`{"name":"earnings","query_config":{"table":"impressions"}}`, adv))
		if rec.Code != http.StatusCreated {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		jobs, _ := store.ListByAccount(context.Background(), rjAccA, 10)
		if jobs[0].QueryConfig.Filters["publisher_id"] != "pub-1" {
			t.Errorf("publisher filter: %v", jobs[0].QueryConfig.Filters)
		}
	})
}

func TestReportJobsListAndGet(t *testing.T) {
	scope := fakeJobScope{}
	store := newFakeReportJobStore()
	adv := rjClaims(rjAccA, "reports:read", "reports:export")

	rec := httptest.NewRecorder()
	reportJobsHandler(store, scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
		`{"name":"mine","query_config":{"table":"impressions"}}`, adv))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed submit: %d", rec.Code)
	}
	var created map[string]string
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"]

	t.Run("list_needs_read_perm", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobsHandler(store, scope, quietLog())(rec, rjReq(http.MethodGet, rjPath, "", rjClaims(rjAccA, "campaigns:read")))
		if rec.Code != http.StatusForbidden {
			t.Errorf("code=%d", rec.Code)
		}
	})

	t.Run("list_own_jobs", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobsHandler(store, scope, quietLog())(rec, rjReq(http.MethodGet, rjPath, "", adv))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "mine") {
			t.Errorf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("get_status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobByIDHandler(store, nil, quietLog())(rec, rjReq(http.MethodGet, rjPath+"/"+id, "", adv))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"queued"`) {
			t.Errorf("code=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("cross_tenant_get_404", func(t *testing.T) {
		other := rjClaims(rjAccB, "reports:read", "reports:export")
		rec := httptest.NewRecorder()
		reportJobByIDHandler(store, nil, quietLog())(rec, rjReq(http.MethodGet, rjPath+"/"+id, "", other))
		if rec.Code != http.StatusNotFound {
			t.Errorf("code=%d, want 404", rec.Code)
		}
	})

	t.Run("download_before_done_409", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobByIDHandler(store, nil, quietLog())(rec, rjReq(http.MethodGet, rjPath+"/"+id+"/download", "", adv))
		if rec.Code != http.StatusConflict {
			t.Errorf("code=%d, want 409", rec.Code)
		}
	})
}

func TestReportJobDownload(t *testing.T) {
	ctx := context.Background()
	scope := fakeJobScope{}
	store := newFakeReportJobStore()
	adv := rjClaims(rjAccA, "reports:read", "reports:export")

	objStore, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Submit, then complete the job the way the worker would.
	rec := httptest.NewRecorder()
	reportJobsHandler(store, scope, quietLog())(rec, rjReq(http.MethodPost, rjPath,
		`{"name":"Spend Report","query_config":{"table":"impressions"}}`, adv))
	var created map[string]string
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"]

	claimed, _ := store.ClaimOne(ctx)
	body := "campaign_id,count\ncamp-1,5\n"
	key := rjAccA + "/" + id + ".csv"
	if err := objStore.Put(ctx, "adtech-reports", key, strings.NewReader(body), int64(len(body)), "text/csv"); err != nil {
		t.Fatal(err)
	}
	store.MarkDone(ctx, claimed.ID, reportjobs.Artifact{Bucket: "adtech-reports", Key: key, Bytes: int64(len(body)), Rows: 1})

	t.Run("download_streams_artifact", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobByIDHandler(store, objStore, quietLog())(rec, rjReq(http.MethodGet, rjPath+"/"+id+"/download", "", adv))
		if rec.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
		}
		if rec.Body.String() != body {
			t.Errorf("body=%q", rec.Body.String())
		}
		if ct := rec.Header().Get(constants.HeaderContentType); ct != "text/csv" {
			t.Errorf("content-type=%q", ct)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="Spend-Report.csv"`) {
			t.Errorf("content-disposition=%q", cd)
		}
	})

	t.Run("download_needs_export_perm", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobByIDHandler(store, objStore, quietLog())(rec, rjReq(http.MethodGet, rjPath+"/"+id+"/download", "",
			rjClaims(rjAccA, "reports:read")))
		if rec.Code != http.StatusForbidden {
			t.Errorf("code=%d", rec.Code)
		}
	})

	t.Run("cross_tenant_download_404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		reportJobByIDHandler(store, objStore, quietLog())(rec, rjReq(http.MethodGet, rjPath+"/"+id+"/download", "",
			rjClaims(rjAccB, "reports:read", "reports:export")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("code=%d", rec.Code)
		}
	})
}

func TestReportJobsDevTenantGuard(t *testing.T) {
	dev := &auth.Claims{AccountID: "dev-account", AccountType: auth.AccountAdmin,
		Permissions: []string{"*"}}
	rec := httptest.NewRecorder()
	reportJobsHandler(newFakeReportJobStore(), fakeJobScope{}, quietLog())(rec, rjReq(http.MethodGet, rjPath, "", dev))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("dev GET code=%d body=%q, want empty list", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	reportJobsHandler(newFakeReportJobStore(), fakeJobScope{}, quietLog())(rec, rjReq(http.MethodPost, rjPath,
		`{"name":"x","query_config":{"table":"impressions"}}`, dev))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("dev POST code=%d, want 400", rec.Code)
	}
}
