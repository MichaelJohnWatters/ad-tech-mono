//go:build e2e

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/catalog"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// ProductUploadResult mirrors the gateway's inline product-feed response.
type ProductUploadResult struct {
	ProductsWritten int `json:"products_written"`
	TotalRows       int `json:"total_rows"`
	ValidRows       int `json:"valid_rows"`
	RejectedRows    int `json:"rejected_rows"`
}

// UploadProductFeed POSTs a product feed CSV to /v1/api/products as the
// account and returns the raw (status, body). Small feeds process inline.
func (h *Harness) UploadProductFeed(t *testing.T, accountID, name, csv string) (int, string) {
	t.Helper()
	email := "prod-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	fw, _ := mw.CreateFormFile("file", "feed.csv")
	_, _ = fw.Write([]byte(csv))
	_ = mw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIProducts, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("product upload: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ListProducts GETs the account's catalog via /v1/api/products.
func (h *Harness) ListProducts(t *testing.T, accountID string) []catalog.Product {
	t.Helper()
	email := "prod-csv-" + accountID + "@e2e.local"
	h.CreateLoginUser(t, accountID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.URLs.Gateway+routes.APIProducts, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("list products: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list products status %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Products []catalog.Product `json:"products"`
		Count    int               `json:"count"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode products: %v (body=%s)", err, string(b))
	}
	return out.Products
}

// ProductViewSKUs returns the SKUs recorded for (account, user) in
// retargeting_product_views (DPA slice 2), live rows only, freshest first —
// a direct DB read for assertions.
func (h *Harness) ProductViewSKUs(t *testing.T, accountID, userID string) []string {
	t.Helper()
	rows, err := h.DB.Query(`
SELECT sku FROM retargeting_product_views
WHERE account_id = $1::uuid AND user_id = $2 AND expires_at > now()
ORDER BY seen_at DESC, sku`, accountID, userID)
	if err != nil {
		t.Fatalf("query product views: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sku string
		if err := rows.Scan(&sku); err != nil {
			t.Fatalf("scan sku: %v", err)
		}
		out = append(out, sku)
	}
	return out
}
