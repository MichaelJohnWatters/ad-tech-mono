package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config/keys"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// adsTxtPolicyResponse tells a publisher exactly how to authorise this platform
// in their ads.txt file. It is derived from the SAME live config the exchange
// enforces on (exchange.adstxt_seller_domain / _id / _enforcement), so the line
// shown here is precisely the line strict mode checks for — no drift between
// "what we tell publishers" and "what we reject them for missing".
type adsTxtPolicyResponse struct {
	Configured     bool   `json:"configured"`       // false when the platform hasn't set its seller identity
	SellerDomain   string `json:"seller_domain"`    // first field of the ads.txt line
	SellerID       string `json:"seller_id"`        // second field
	Relationship   string `json:"relationship"`     // DIRECT — publishers sell to us directly
	Line           string `json:"line"`             // the full line to paste, when configured
	Enforcement    string `json:"enforcement"`      // off | warn | strict (live)
	SellersJSONURL string `json:"sellers_json_url"` // our IAB sellers.json, the counterpart record
}

// integrationAdsTxtHandler serves GET /v1/api/integration/adstxt: the ads.txt
// authorisation policy a publisher needs to add so this platform passes the
// exchange's seller-authorisation check. JWT-gated (publishers are logged-in
// tenant users); the data itself is public.
func integrationAdsTxtHandler(cfg *config.Config, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if middleware.ClaimsFromContext(r.Context()) == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		domain := strings.TrimSpace(keys.Exchange.AdsTxtSellerDomain.Get(cfg))
		sellerID := strings.TrimSpace(keys.Exchange.AdsTxtSellerID.Get(cfg))
		mode := strings.ToLower(strings.TrimSpace(keys.Exchange.AdsTxtEnforcement.Get(cfg)))
		if mode == "" {
			mode = "off"
		}
		resp := adsTxtPolicyResponse{
			SellerDomain:   domain,
			SellerID:       sellerID,
			Relationship:   "DIRECT",
			Enforcement:    mode,
			SellersJSONURL: routes.SellersJSON,
		}
		if domain != "" && sellerID != "" {
			resp.Configured = true
			resp.Line = domain + ", " + sellerID + ", DIRECT"
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
