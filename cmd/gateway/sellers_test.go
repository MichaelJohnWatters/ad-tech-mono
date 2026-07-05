package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/fraud"
)

type fakeSellerSource struct {
	sellers []fraud.SellerEntry
	err     error
}

func (f fakeSellerSource) Sellers(context.Context) ([]fraud.SellerEntry, error) {
	return f.sellers, f.err
}

func decodeSellers(t *testing.T, body io.Reader) fraud.SellersJSON {
	t.Helper()
	var out fraud.SellersJSON
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode sellers.json: %v", err)
	}
	return out
}

func TestSellersJSONHandler_FromPublishers(t *testing.T) {
	src := fakeSellerSource{sellers: []fraud.SellerEntry{
		{SellerID: "id-1", Name: "Daily News", Domain: "daily-news.com", SellerType: "PUBLISHER"},
		{SellerID: "id-2", Name: "Tech Review", Domain: "tech-review.io", SellerType: "PUBLISHER"},
	}}
	rr := httptest.NewRecorder()
	sellersJSONHandler(src, quietLog()).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sellers.json", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	out := decodeSellers(t, rr.Body)
	if len(out.Sellers) != 2 {
		t.Fatalf("expected 2 sellers, got %d", len(out.Sellers))
	}
	if out.Sellers[0].SellerID != "id-1" || out.Sellers[0].Domain != "daily-news.com" {
		t.Errorf("unexpected first seller: %+v", out.Sellers[0])
	}
	if out.ContactEmail == "" || out.Version == "" {
		t.Errorf("sellers.json missing contact/version metadata: %+v", out)
	}
}

func TestSellersJSONHandler_ErrorServesEmptyList(t *testing.T) {
	src := fakeSellerSource{err: errors.New("db down")}
	rr := httptest.NewRecorder()
	sellersJSONHandler(src, quietLog()).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sellers.json", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.Bytes()
	if !json.Valid(body) {
		t.Fatalf("body not valid JSON: %s", body)
	}
	var out fraud.SellersJSON
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Sellers) != 0 {
		t.Errorf("expected empty seller list on error, got %d", len(out.Sellers))
	}
	// The sellers array must serialize as [] not null.
	if !strings.Contains(string(body), `"sellers":[]`) {
		t.Errorf("expected empty array, got: %s", body)
	}
}
