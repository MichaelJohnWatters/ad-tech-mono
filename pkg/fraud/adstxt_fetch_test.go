package fraud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchAdsTxt(t *testing.T) {
	const adsTxt = `# comment
adtech.example, pub-123, DIRECT, tag-1
google.com, 456, RESELLER
`
	t.Run("valid", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ads.txt" {
				t.Errorf("unexpected path %s", r.URL.Path)
			}
			w.Write([]byte(adsTxt))
		}))
		defer srv.Close()

		rec := FetchAdsTxt(context.Background(), srv.Client(), hostOf(srv.URL))
		if rec.Status != "valid" {
			t.Fatalf("status = %q, want valid", rec.Status)
		}
		if len(rec.Entries) != 2 {
			t.Fatalf("entries = %d, want 2 (comment skipped)", len(rec.Entries))
		}
		if rec.Entries[0].Domain != "adtech.example" || rec.Entries[0].AccountID != "pub-123" || rec.Entries[0].Relationship != "DIRECT" {
			t.Errorf("entry[0] = %+v", rec.Entries[0])
		}
	})

	t.Run("missing", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()
		rec := FetchAdsTxt(context.Background(), srv.Client(), hostOf(srv.URL))
		if rec.Status != "missing" {
			t.Fatalf("status = %q, want missing", rec.Status)
		}
	})

	t.Run("error", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		rec := FetchAdsTxt(context.Background(), srv.Client(), hostOf(srv.URL))
		if rec.Status != "error" {
			t.Fatalf("status = %q, want error", rec.Status)
		}
	})
}

// hostOf strips the scheme from an httptest URL so FetchAdsTxt re-adds
// https:// (the test client trusts the test server's cert regardless).
func hostOf(u string) string {
	return u[len("https://"):]
}
