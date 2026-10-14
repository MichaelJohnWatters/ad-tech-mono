package main

import (
	"testing"
	"time"
)

func TestAuctionDeadline(t *testing.T) {
	ms := time.Millisecond
	cases := []struct {
		name       string
		bidTimeout time.Duration
		tmax       int
		margin     time.Duration
		want       time.Duration
	}{
		{"no tmax = bid_timeout", 500 * ms, 0, 25 * ms, 500 * ms},
		{"tighter tmax wins, minus margin", 500 * ms, 200, 25 * ms, 175 * ms},
		{"looser tmax capped by bid_timeout", 500 * ms, 2000, 25 * ms, 500 * ms},
		{"default ssp.tmax keeps 500ms", 500 * ms, 525, 25 * ms, 500 * ms},
		{"tmax under margin floors", 500 * ms, 20, 25 * ms, tmaxFloor},
		{"no bid_timeout = tmax budget", 0, 300, 25 * ms, 275 * ms},
	}
	for _, c := range cases {
		if got := auctionDeadline(c.bidTimeout, c.tmax, c.margin); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseChannelTimeouts(t *testing.T) {
	m, errs := parseChannelTimeouts(" display=250ms, VIDEO=600ms ,bogus,audio=-5ms,native=abc,,ctv=1s,dispaly=200ms")
	if len(errs) != 4 {
		t.Errorf("want 4 malformed entries (bogus, negative, non-duration, unknown channel), got %d: %v", len(errs), errs)
	}
	want := map[string]time.Duration{"display": 250 * time.Millisecond, "video": 600 * time.Millisecond, "ctv": time.Second}
	if len(m) != len(want) {
		t.Fatalf("got %v, want %v", m, want)
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s: got %v, want %v", k, m[k], v)
		}
	}
}

func TestChannelBidTimeoutAndCache(t *testing.T) {
	ms := time.Millisecond
	ct := loadChannelTimeouts("display=250ms,video=800ms", nil)
	if ct.max != 800*ms {
		t.Errorf("max override: got %v, want 800ms (sizes the HTTP backstop)", ct.max)
	}
	if got := channelBidTimeout(ct, "display", 500*ms); got != 250*ms {
		t.Errorf("display override: got %v", got)
	}
	if got := channelBidTimeout(ct, "native", 500*ms); got != 500*ms {
		t.Errorf("unlisted channel must fall back to bid_timeout: got %v", got)
	}
	if again := loadChannelTimeouts("display=250ms,video=800ms", nil); again != ct {
		t.Error("unchanged raw value must reuse the cached parse")
	}
	// tmax still applies on top of a per-channel value.
	if got := auctionDeadline(channelBidTimeout(ct, "video", 500*ms), 300, 25*ms); got != 275*ms {
		t.Errorf("video 800ms capped by tmax 300-25: got %v", got)
	}
	if empty := loadChannelTimeouts("", nil); channelBidTimeout(empty, "display", 500*ms) != 500*ms {
		t.Error("empty config = global bid_timeout")
	}
}
