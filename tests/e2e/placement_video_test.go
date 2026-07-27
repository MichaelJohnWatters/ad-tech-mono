//go:build e2e

// Per-placement video settings via the real API. The SSP applies a placement's
// video_config (buildVideoImp) when building a video bid request; this proves
// the config path — create/patch write it, the placement list returns it, and
// validation rejects bad values.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestPlacementVideoConfigViaAPI(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	h.Reset(t)

	uniq := fmt.Sprintf("vid-%d", time.Now().UnixNano())
	pub := h.Signup(t, "Vid Pub", uniq+"-pub@api.test", "pw-e2e-1", "publisher")
	site := h.APIJSON(t, pub, http.MethodPost, "/v1/api/publishers", `{"name":"Vid Site","domain":"`+uniq+`.test"}`)

	// Create a video placement with a non-skippable 15–30s pre-roll config.
	pl := h.APIJSON(t, pub, http.MethodPost, "/v1/api/placements", fmt.Sprintf(`{
		"publisher_id":%q,"name":"Vid Preroll","format":"video","width":640,"height":360,"floor_price":5.0,
		"video_config":{"skippable":false,"min_duration":15,"max_duration":30,"plcmt":3,"mimes":["video/mp4"]}
	}`, site["id"]))
	placementID := pl["id"].(string)

	// List it back — the SSP warm cache returns video_config on the placement.
	findVideo := func() map[string]any {
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+"/v1/api/placements", nil)
		resp, _ := pub.Do(req)
		var pls []map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&pls)
		resp.Body.Close()
		for _, p := range pls {
			if p["ID"] == placementID {
				vc, _ := p["VideoConfig"].(map[string]any)
				return vc
			}
		}
		return nil
	}
	// The create/patch publish an async cache-invalidate that fans out to every
	// SSP replica; the warm caches are eventually consistent, and the list GET
	// (behind the LB) can land on a pod that hasn't reloaded yet. Poll until the
	// expected config converges across pods rather than asserting on one shot.
	pollVideo := func(want func(map[string]any) bool) map[string]any {
		t.Helper()
		h.RefreshAllCaches(t)
		deadline := time.Now().Add(15 * time.Second)
		var last map[string]any
		for time.Now().Before(deadline) {
			if vc := findVideo(); vc != nil {
				last = vc
				if want(vc) {
					return vc
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		return last
	}

	vc := pollVideo(func(vc map[string]any) bool {
		return vc["skippable"] == false && vc["min_duration"] == float64(15) && vc["plcmt"] == float64(3)
	})
	if vc == nil {
		t.Fatalf("placement %s not found / no VideoConfig", placementID)
	}
	if vc["skippable"] != false || vc["min_duration"].(float64) != 15 || vc["plcmt"].(float64) != 3 {
		t.Errorf("video_config round-trip = %v, want skippable=false min=15 plcmt=3", vc)
	}

	// PATCH the config (skippable, longer window).
	h.APIJSON(t, pub, http.MethodPatch, "/v1/api/placements/"+placementID,
		`{"video_config":{"skippable":true,"skip_after":5,"min_duration":6,"max_duration":60}}`)
	vc = pollVideo(func(vc map[string]any) bool {
		return vc["skippable"] == true && vc["max_duration"] == float64(60)
	})
	if vc == nil || vc["skippable"] != true || vc["max_duration"].(float64) != 60 {
		t.Errorf("after patch video_config = %v, want skippable=true max=60", vc)
	}

	// Validation: min > max is rejected.
	if code := h.APIStatus(t, pub, http.MethodPatch, "/v1/api/placements/"+placementID,
		`{"video_config":{"min_duration":40,"max_duration":30}}`); code != http.StatusBadRequest {
		t.Errorf("min>max video_config = %d, want 400", code)
	}
	// Validation: bad plcmt rejected.
	if code := h.APIStatus(t, pub, http.MethodPatch, "/v1/api/placements/"+placementID,
		`{"video_config":{"plcmt":9}}`); code != http.StatusBadRequest {
		t.Errorf("bad plcmt = %d, want 400", code)
	}
}
