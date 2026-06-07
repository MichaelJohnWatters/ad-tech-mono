package main

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAppendTraceQuery(t *testing.T) {
	t.Run("no existing query → adds with ?", func(t *testing.T) {
		got := appendTraceQuery("https://example.com/landing", "abc-123")
		want := "https://example.com/landing?adtech_tid=abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("existing query → adds with &", func(t *testing.T) {
		got := appendTraceQuery("https://example.com/landing?utm=a", "abc-123")
		want := "https://example.com/landing?utm=a&adtech_tid=abc-123"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("idempotent when adtech_tid already present", func(t *testing.T) {
		// A redir URL that already carries adtech_tid (e.g. because the
		// tracker is chained behind another redirect) should not double
		// up. Idempotency keeps the landing-side analytics clean.
		in := "https://example.com/landing?adtech_tid=existing&utm=a"
		if got := appendTraceQuery(in, "fresh"); got != in {
			t.Errorf("got %q, want unchanged %q", got, in)
		}
	})
	t.Run("empty inputs are passthroughs", func(t *testing.T) {
		if got := appendTraceQuery("", "x"); got != "" {
			t.Errorf("empty redir should stay empty, got %q", got)
		}
		if got := appendTraceQuery("https://example.com/", ""); got != "https://example.com/" {
			t.Errorf("empty trace should leave redir untouched, got %q", got)
		}
	})
	t.Run("trace_id is url-encoded", func(t *testing.T) {
		// Trace IDs are 32-hex in production but defensive: if someone
		// passes a value with reserved chars it should round-trip via
		// url.QueryEscape so the redirect stays valid.
		got := appendTraceQuery("https://example.com/", "a b&c")
		want := "https://example.com/?adtech_tid=" + url.QueryEscape("a b&c")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestIsExpired(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)

	t.Run("no exp param → not expired", func(t *testing.T) {
		// Legacy URLs without exp pass through. Tracker policy is
		// "if you set exp, we honour it; otherwise no expiry".
		q := url.Values{}
		if isExpired(q, now) {
			t.Errorf("missing exp should never be expired")
		}
	})
	t.Run("empty exp value → not expired", func(t *testing.T) {
		q := url.Values{"exp": []string{""}}
		if isExpired(q, now) {
			t.Errorf("empty exp should not be expired")
		}
	})
	t.Run("future exp → not expired", func(t *testing.T) {
		future := now.Add(1 * time.Hour).Unix()
		q := url.Values{"exp": []string{itoa(future)}}
		if isExpired(q, now) {
			t.Errorf("future exp should not be expired")
		}
	})
	t.Run("past exp → expired", func(t *testing.T) {
		past := now.Add(-1 * time.Hour).Unix()
		q := url.Values{"exp": []string{itoa(past)}}
		if !isExpired(q, now) {
			t.Errorf("past exp should be expired")
		}
	})
	t.Run("within +5s skew tolerance → not expired", func(t *testing.T) {
		// Per the implementation comment: client clocks drift forward.
		// A URL with exp 3 seconds before now should still be honoured
		// to avoid rejecting legitimately-fresh clicks.
		justPast := now.Add(-3 * time.Second).Unix()
		q := url.Values{"exp": []string{itoa(justPast)}}
		if isExpired(q, now) {
			t.Errorf("exp within +5s skew should not be expired")
		}
	})
	t.Run("just outside skew → expired", func(t *testing.T) {
		outside := now.Add(-6 * time.Second).Unix()
		q := url.Values{"exp": []string{itoa(outside)}}
		if !isExpired(q, now) {
			t.Errorf("exp 6s past should be expired (skew is only +5s)")
		}
	})
	t.Run("malformed exp → not expired (sig path catches it)", func(t *testing.T) {
		// Defensive: junk values shouldn't trigger an expiry rejection.
		// The signature path is the right place to reject malformed
		// data; an unparseable exp here just means "no expiry policy
		// on this URL".
		q := url.Values{"exp": []string{"not-a-number"}}
		if isExpired(q, now) {
			t.Errorf("unparseable exp should not trigger expiry")
		}
	})
}

// Local itoa avoids importing strconv just for tests.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	out := string(b[i:])
	if neg {
		out = "-" + out
	}
	return strings.TrimLeft(out, "")
}
