//go:build e2e

// Advertiser retargeting-audience control: the portal lets an advertiser CREATE a
// retargeting audience (name + pixel tag + window) via POST /v1/api/audiences/
// retargeting, and the created segment is a REAL one that cmd/audience-rt enrolls
// into. Proves the API create + list (with live enrollment count) and that a pixel
// visit for the chosen tag lands in the count — the whole control wired end to end.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/tests/e2e/harness"
)

func TestAdvertiserRetargetingAudienceControl(t *testing.T) {
	h := harness.WaitReady(t, 60*time.Second)
	w := harness.BuildBasicWorld(t, h, "rt-portal")
	uniq := time.Now().UnixNano()
	tag := fmt.Sprintf("pdp-%d", uniq)

	email := "rt-portal-" + w.AdvAcc.ID + "@e2e.local"
	h.CreateLoginUser(t, w.AdvAcc.ID, email, "e2e-pass", "owner")
	client := h.LoginAs(t, email, "e2e-pass")

	type rtSeg struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Tag        string `json:"tag"`
		WindowDays int    `json:"window_days"`
		Members    int    `json:"members"`
	}
	list := func() []rtSeg {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URLs.Gateway+routes.APIAudienceRetargeting, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("list retargeting: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("list status %d: %s", resp.StatusCode, b)
		}
		var out []rtSeg
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return out
	}
	find := func() *rtSeg {
		for i := range list() {
			if s := list()[i]; s.Tag == tag {
				return &s
			}
		}
		return nil
	}

	// Create a retargeting audience through the portal API.
	body, _ := json.Marshal(map[string]any{"name": "Cart abandoners", "tag": tag, "window_days": 14})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URLs.Gateway+routes.APIAudienceRetargeting, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create retargeting: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d: %s", resp.StatusCode, rb)
	}

	// It lists with the tag/window it was created with, and zero enrollment.
	seg := find()
	if seg == nil {
		t.Fatal("created retargeting audience not returned by list")
	}
	if seg.WindowDays != 14 {
		t.Errorf("window = %d, want 14", seg.WindowDays)
	}
	if seg.Members != 0 {
		t.Errorf("fresh audience already has %d members, want 0", seg.Members)
	}

	// A visit for this tag enrols via cmd/audience-rt → the portal's live count
	// reflects it within seconds (proving the portal-created segment is real).
	fireVisit(t, w.AdvAcc.ID, tag, fmt.Sprintf("rtp-visitor-%d", uniq))
	deadline := time.Now().Add(30 * time.Second)
	for {
		if s := find(); s != nil && s.Members >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("enrollment count never reflected the pixel visit")
		}
		time.Sleep(500 * time.Millisecond)
	}
}
