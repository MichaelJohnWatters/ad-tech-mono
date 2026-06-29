package fraud

import "testing"

func TestReplaceBlocklists_DBSourced(t *testing.T) {
	c := NewRealTimeChecker(DefaultConfig())

	// Before loading: a custom IP and a custom UA pattern are clean.
	if r := c.Check(Request{IP: "6.6.6.6", UserAgent: "Mozilla/5.0", Referer: "x"}); r.Blocked {
		t.Fatalf("pre-load: 6.6.6.6 should not be blocked")
	}
	if r := c.Check(Request{IP: "1.2.3.4", UserAgent: "evilcorp-scanner/1.0", Referer: "x"}); r.Blocked {
		t.Fatalf("pre-load: evilcorp-scanner should not be blocked")
	}

	c.ReplaceBlocklists([]BlocklistEntry{
		{Type: "ip", Value: "6.6.6.6"},
		{Type: "ua", Value: "evilcorp-scanner"},
		{Type: "domain", Value: "ignored.example"}, // non-ip/ua ignored here
	})

	if r := c.Check(Request{IP: "6.6.6.6", UserAgent: "Mozilla/5.0", Referer: "x"}); !r.Blocked {
		t.Errorf("post-load: 6.6.6.6 should be blocked, reasons=%v", r.Reasons)
	}
	if r := c.Check(Request{IP: "1.2.3.4", UserAgent: "EvilCorp-Scanner/1.0", Referer: "x"}); !r.Blocked {
		t.Errorf("post-load: evilcorp-scanner UA should block case-insensitively, reasons=%v", r.Reasons)
	}

	// A refresh with an empty set clears the DB blocklist (row removed in DB).
	c.ReplaceBlocklists(nil)
	if r := c.Check(Request{IP: "6.6.6.6", UserAgent: "Mozilla/5.0", Referer: "x"}); r.Blocked {
		t.Errorf("post-clear: 6.6.6.6 should no longer be blocked")
	}

	// Hardcoded bot patterns still work regardless of DB state.
	if r := c.Check(Request{IP: "8.8.8.8", UserAgent: "Googlebot/2.1", Referer: "x"}); !r.Blocked {
		t.Errorf("hardcoded Googlebot pattern should still block")
	}
}
