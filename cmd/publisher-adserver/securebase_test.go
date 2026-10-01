package main

import (
	"net/http/httptest"
	"testing"
)

const secureTestBase = "https://gateway.adtech.local"

func TestSecureRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/pubad/serve", nil)
	if secureRequest(r) {
		t.Error("plain request must not be treated as secure")
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !secureRequest(r) {
		t.Error("X-Forwarded-Proto: https must be treated as secure")
	}
	r.Header.Set("X-Forwarded-Proto", "HTTPS") // case-insensitive
	if !secureRequest(r) {
		t.Error("X-Forwarded-Proto is case-insensitive")
	}
	r.Header.Set("X-Forwarded-Proto", "http")
	if secureRequest(r) {
		t.Error("X-Forwarded-Proto: http must NOT be secure")
	}
}

func TestSecureTrackerBase(t *testing.T) {
	const tracker = "http://localhost:8080"
	plain := httptest.NewRequest("GET", "/", nil)
	if got := secureTrackerBase(plain, tracker, secureTestBase); got != tracker {
		t.Errorf("plain request: got %q, want unchanged %q", got, tracker)
	}
	sec := httptest.NewRequest("GET", "/", nil)
	sec.Header.Set("X-Forwarded-Proto", "https")
	if got := secureTrackerBase(sec, tracker, secureTestBase); got != secureTestBase {
		t.Errorf("https request: got %q, want %q", got, secureTestBase)
	}
}

func TestRewriteHostIfSecure(t *testing.T) {
	const raw = "http://localhost:8080/v1/t/imp?cid=abc&sig=deadbeef&tid=xyz"
	const want = "https://gateway.adtech.local/v1/t/imp?cid=abc&sig=deadbeef&tid=xyz"

	plain := httptest.NewRequest("GET", "/", nil)
	if got := rewriteHostIfSecure(plain, raw, secureTestBase); got != raw {
		t.Errorf("plain: got %q, want unchanged %q", got, raw)
	}

	sec := httptest.NewRequest("GET", "/", nil)
	sec.Header.Set("X-Forwarded-Proto", "https")
	if got := rewriteHostIfSecure(sec, raw, secureTestBase); got != want {
		t.Errorf("https: got %q, want %q", got, want)
	}
}

func TestRewriteHostPreservesPathAndQueryOrder(t *testing.T) {
	// The HMAC covers path + params; swapping only scheme+host must leave the
	// path and the raw query string byte-identical so the signature holds.
	const raw = "http://localhost:8080/v1/t/imp?b=2&a=1&sig=zzz"
	const want = "https://gateway.adtech.local/v1/t/imp?b=2&a=1&sig=zzz"
	if got := rewriteHost(raw, secureTestBase); got != want {
		t.Errorf("got %q, want %q (path+query must be preserved verbatim)", got, want)
	}
}

func TestRewriteHostPassthrough(t *testing.T) {
	// relative URL / empty / bad secure base → unchanged
	if got := rewriteHost("/relative/path?x=1", secureTestBase); got != "/relative/path?x=1" {
		t.Errorf("relative URL must pass through, got %q", got)
	}
	if got := rewriteHost("", secureTestBase); got != "" {
		t.Errorf("empty must pass through, got %q", got)
	}
	if got := rewriteHost("http://localhost:8080/x", "://bad"); got != "http://localhost:8080/x" {
		t.Errorf("bad secure base must pass through, got %q", got)
	}
}

func TestRewriteBaseIfSecure(t *testing.T) {
	const oldBase = "http://localhost:8080"
	// HTML with an asset <img>, a baked beacon, AND a URL-ENCODED redir param
	// that must NOT be rewritten (it's signed material inside a signed URL).
	body := `<img src="http://localhost:8080/v1/creatives/x.png">` +
		`<img src="http://localhost:8080/v1/t/imp?sig=abc&redir=http%3A%2F%2Flocalhost%3A8080%2Fland">`

	plain := httptest.NewRequest("GET", "/", nil)
	if got := rewriteBaseIfSecure(plain, body, oldBase, secureTestBase); got != body {
		t.Error("plain request must leave the body unchanged")
	}

	sec := httptest.NewRequest("GET", "/", nil)
	sec.Header.Set("X-Forwarded-Proto", "https")
	got := rewriteBaseIfSecure(sec, body, oldBase, secureTestBase)
	want := `<img src="https://gateway.adtech.local/v1/creatives/x.png">` +
		`<img src="https://gateway.adtech.local/v1/t/imp?sig=abc&redir=http%3A%2F%2Flocalhost%3A8080%2Fland">`
	if got != want {
		t.Errorf("https rewrite:\n got  %q\n want %q", got, want)
	}
}
