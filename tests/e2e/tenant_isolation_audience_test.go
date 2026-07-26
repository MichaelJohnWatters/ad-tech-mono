//go:build e2e

// Audience-segment list isolation: the /v1/api/audiences feed is scoped to the
// caller's account (cmd/gateway/audiences.go — ListSegments(claims.AccountID)),
// so advertiser B must never see advertiser A's segments. Campaign isolation is
// covered in tenant_isolation_api_test.go; this extends the same guarantee to
// the audience surface, which had no cross-tenant assertion.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestTenantIsolationAudienceList(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)

	uniq := fmt.Sprintf("isoseg-%d", time.Now().UnixNano())
	accA := h.CreateAdvertiser(t, "isoseg-a-"+uniq)
	accB := h.CreateAdvertiser(t, "isoseg-b-"+uniq)

	// A owns a segment; B owns none.
	segName := "seg-secret-" + uniq
	segID := h.CreateSegment(t, accA, segName)

	// A real login session for each account.
	pw := "pw-e2e-1"
	emailA := uniq + "-a@api.test"
	emailB := uniq + "-b@api.test"
	h.CreateLoginUser(t, accA.ID, emailA, pw, "owner")
	h.CreateLoginUser(t, accB.ID, emailB, pw, "owner")
	clientA := h.LoginAs(t, emailA, pw)
	clientB := h.LoginAs(t, emailB, pw)

	listSegments := func(t *testing.T, client *http.Client) []map[string]any {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/audiences", nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("list audiences: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list audiences status = %d", resp.StatusCode)
		}
		var segs []map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&segs)
		return segs
	}

	has := func(segs []map[string]any) bool {
		for _, s := range segs {
			if s["id"] == segID || s["name"] == segName {
				return true
			}
		}
		return false
	}

	t.Run("owner_sees_own_segment", func(t *testing.T) {
		if !has(listSegments(t, clientA)) {
			t.Errorf("account A does not see its own segment %s (%s)", segID, segName)
		}
	})

	t.Run("other_tenant_cannot_see_segment", func(t *testing.T) {
		if has(listSegments(t, clientB)) {
			t.Errorf("account B can see account A's segment %s — tenant isolation broken", segID)
		}
	})
}
