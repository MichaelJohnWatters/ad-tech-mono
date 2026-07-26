//go:build e2e

// The codebase mandates parameterised queries only (no string concatenation).
// This proves it at the live boundary: a campaign name carrying a SQL-injection
// payload is stored and returned as a literal string, the target table is
// untouched, and the API keeps working. A regression to string-built SQL would
// either execute the payload or corrupt the row — both caught here.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestCampaignNameInjectionSafe(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("inj-%d", time.Now().UnixNano())
	adv := h.Signup(t, "Injection Adv", uniq+"@api.test", "pw-e2e-1", "advertiser")

	// A name that would drop the target table if it were concatenated into SQL.
	payload := "Robert'); DROP TABLE line_items;-- " + uniq
	created := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns",
		fmt.Sprintf(`{"name":%q,"base_bid":1.5,"daily_budget":100,"bid_strategy":"cpm"}`, payload))
	campaignID, _ := created["id"].(string)
	if campaignID == "" {
		t.Fatalf("create returned no id: %v", created)
	}

	// The row exists and the name is stored VERBATIM — if line_items had been
	// dropped this query errors; if the payload had executed the name would
	// differ. (h.DB is the BYPASSRLS dev role — no tenant GUC needed.)
	var name string
	if err := h.DB.QueryRow(`SELECT name FROM line_items WHERE id = $1::uuid`, campaignID).Scan(&name); err != nil {
		t.Fatalf("read back campaign (line_items may be damaged): %v", err)
	}
	if name != payload {
		t.Errorf("stored name = %q, want the literal payload %q (injection not neutralised)", name, payload)
	}

	// The API still works afterward — a second create+read proves the table and
	// connection survived the payload.
	created2 := h.APIJSON(t, adv, http.MethodPost, "/v1/api/campaigns",
		`{"name":"after injection","base_bid":1,"daily_budget":10,"bid_strategy":"cpm"}`)
	if id2, _ := created2["id"].(string); id2 == "" {
		t.Errorf("second create failed after the injection payload: %v", created2)
	}
}
