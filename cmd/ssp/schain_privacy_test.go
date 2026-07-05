package main

import (
	"net/http/httptest"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
)

func TestOriginSChain(t *testing.T) {
	t.Run("unset seller domain omits schain", func(t *testing.T) {
		if got := originSChain("", "pub-1", "trace-1"); got != nil {
			t.Fatalf("expected nil source when seller domain unset, got %+v", got)
		}
	})

	t.Run("builds valid complete one-node chain", func(t *testing.T) {
		src := originSChain("adtech.example", "pub-42", "trace-abc")
		if src == nil {
			t.Fatal("expected a source, got nil")
		}
		sc := src.Ext.SChain
		if err := openrtb.ValidateSChain(sc); err != nil {
			t.Fatalf("originated schain failed validation: %v", err)
		}
		if sc.Complete != 1 {
			t.Errorf("complete = %d, want 1", sc.Complete)
		}
		if len(sc.Nodes) != 1 {
			t.Fatalf("nodes = %d, want 1", len(sc.Nodes))
		}
		n := sc.Nodes[0]
		if n.ASI != "adtech.example" {
			t.Errorf("asi = %q, want adtech.example", n.ASI)
		}
		if n.SID != "pub-42" {
			t.Errorf("sid = %q, want pub-42 (publisher seller id)", n.SID)
		}
		if n.HP != 1 {
			t.Errorf("hp = %d, want 1", n.HP)
		}
	})
}

func TestApplyPrivacySignals(t *testing.T) {
	t.Run("no signals leaves request minimal", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/serve?placement_id=x", nil)
		bidReq := openrtb.BidRequest{ID: "1"}
		applyPrivacySignals(req, &bidReq)
		if bidReq.Regs != nil {
			t.Errorf("expected no Regs for a signal-free request, got %+v", bidReq.Regs)
		}
		if bidReq.User != nil {
			t.Errorf("expected no User for a signal-free request, got %+v", bidReq.User)
		}
	})

	t.Run("gdpr + consent populate regs and user", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/serve?gdpr=1&gdpr_consent=CONSENT123&us_privacy=1YNN", nil)
		bidReq := openrtb.BidRequest{ID: "1"}
		applyPrivacySignals(req, &bidReq)
		if bidReq.Regs == nil || bidReq.Regs.Ext == nil {
			t.Fatal("expected Regs.Ext populated")
		}
		if bidReq.Regs.Ext.GDPR != 1 {
			t.Errorf("gdpr = %d, want 1", bidReq.Regs.Ext.GDPR)
		}
		if bidReq.Regs.Ext.USPrivacy != "1YNN" {
			t.Errorf("us_privacy = %q, want 1YNN", bidReq.Regs.Ext.USPrivacy)
		}
		if bidReq.User == nil || bidReq.User.Ext == nil || bidReq.User.Ext.Consent != "CONSENT123" {
			t.Errorf("expected consent CONSENT123 on user ext, got %+v", bidReq.User)
		}
	})

	t.Run("consent query param alias works", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/serve?consent=TCFSTRING", nil)
		bidReq := openrtb.BidRequest{ID: "1"}
		applyPrivacySignals(req, &bidReq)
		if bidReq.User == nil || bidReq.User.Ext == nil || bidReq.User.Ext.Consent != "TCFSTRING" {
			t.Errorf("expected consent TCFSTRING via ?consent= alias, got %+v", bidReq.User)
		}
	})

	t.Run("Sec-GPC header sets first-class gpc flag", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/serve", nil)
		req.Header.Set("Sec-GPC", "1")
		bidReq := openrtb.BidRequest{ID: "1"}
		applyPrivacySignals(req, &bidReq)
		if bidReq.Regs == nil || bidReq.Regs.Ext == nil {
			t.Fatal("expected Regs.Ext populated from GPC")
		}
		if bidReq.Regs.Ext.GPC != 1 {
			t.Errorf("gpc = %d, want 1", bidReq.Regs.Ext.GPC)
		}
		// GPC no longer fabricates a us_privacy string — it's carried distinctly.
		if bidReq.Regs.Ext.USPrivacy != "" {
			t.Errorf("us_privacy = %q, want empty (GPC is first-class, not a USP hack)", bidReq.Regs.Ext.USPrivacy)
		}
	})

	t.Run("?gpc=1 query param sets gpc flag", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/serve?gpc=1", nil)
		bidReq := openrtb.BidRequest{ID: "1"}
		applyPrivacySignals(req, &bidReq)
		if bidReq.Regs == nil || bidReq.Regs.Ext.GPC != 1 {
			t.Errorf("expected gpc=1 from query param, got %+v", bidReq.Regs)
		}
	})

	t.Run("gpc and explicit us_privacy are independent", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/serve?us_privacy=1YNN", nil)
		req.Header.Set("Sec-GPC", "1")
		bidReq := openrtb.BidRequest{ID: "1"}
		applyPrivacySignals(req, &bidReq)
		if bidReq.Regs.Ext.USPrivacy != "1YNN" {
			t.Errorf("us_privacy = %q, want 1YNN preserved", bidReq.Regs.Ext.USPrivacy)
		}
		if bidReq.Regs.Ext.GPC != 1 {
			t.Errorf("gpc = %d, want 1 (carried alongside us_privacy)", bidReq.Regs.Ext.GPC)
		}
	})

	t.Run("gpp passthrough", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/serve?gpp=DBABMA~1YNN&gpp_sid=7,8", nil)
		bidReq := openrtb.BidRequest{ID: "1"}
		applyPrivacySignals(req, &bidReq)
		if bidReq.Regs == nil || bidReq.Regs.Ext.GPP != "DBABMA~1YNN" {
			t.Errorf("expected gpp carried through, got %+v", bidReq.Regs)
		}
		if bidReq.Regs.Ext.GPPSID != "7,8" {
			t.Errorf("gpp_sid = %q, want 7,8", bidReq.Regs.Ext.GPPSID)
		}
	})
}
