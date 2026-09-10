package sdkasset

import "testing"

func TestNewParsesVersionAndDerives(t *testing.T) {
	a, err := New([]byte("(function(){ var SDK_VERSION = '2.3.1'; })();"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Version != "2.3.1" || a.Major != "v2" {
		t.Errorf("version=%q major=%q, want 2.3.1/v2", a.Version, a.Major)
	}
	if a.Integrity[:7] != "sha384-" || len(a.Integrity) <= 7 {
		t.Errorf("integrity=%q, want sha384-…", a.Integrity)
	}
	if a.ETag == "" || a.ETag[0] != '"' {
		t.Errorf("etag=%q, want a quoted strong etag", a.ETag)
	}
	if a.PinnedURL() != "/sdk/2.3.1/adtech.js" || a.MajorURL() != "/sdk/v2/adtech.js" || a.LatestURL() != "/sdk/latest/adtech.js" {
		t.Errorf("urls: %s / %s / %s", a.PinnedURL(), a.MajorURL(), a.LatestURL())
	}
}

func TestNewRejectsMissingVersion(t *testing.T) {
	if _, err := New([]byte("no version here")); err == nil {
		t.Error("want error when SDK_VERSION is absent")
	}
}

func TestIntegrityChangesWithBytes(t *testing.T) {
	a, _ := New([]byte("var SDK_VERSION = '1.0.0'; // a"))
	b, _ := New([]byte("var SDK_VERSION = '1.0.0'; // b"))
	if a.Integrity == b.Integrity || a.ETag == b.ETag {
		t.Error("integrity/etag must change when the bytes change")
	}
}

func TestResolveChannels(t *testing.T) {
	a, _ := New([]byte("var SDK_VERSION = '2.0.0';"))
	for _, tc := range []struct {
		seg    string
		wantCh Channel
		wantOK bool
	}{
		{"2.0.0", ChannelPinned, true},
		{"v2", ChannelMajor, true},
		{"latest", ChannelLatest, true},
		{"v1", 0, false},    // a major we don't host
		{"2.0.1", 0, false}, // an exact version we don't ship
		{"garbage", 0, false},
	} {
		ch, ok := a.Resolve(tc.seg)
		if ok != tc.wantOK || (ok && ch != tc.wantCh) {
			t.Errorf("Resolve(%q)=(%v,%v), want (%v,%v)", tc.seg, ch, ok, tc.wantCh, tc.wantOK)
		}
	}
}

func TestCacheControl(t *testing.T) {
	if CacheControl(ChannelPinned) != "public, max-age=31536000, immutable" {
		t.Error("pinned must be immutable 1y")
	}
	if CacheControl(ChannelMajor) != "public, max-age=3600" {
		t.Error("major must be 1h")
	}
	if CacheControl(ChannelLatest) != "public, max-age=300" {
		t.Error("latest must be short")
	}
}
