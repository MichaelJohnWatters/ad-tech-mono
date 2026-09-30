// Command securitydemo is a HOST DEMO TOOL (never deployed) that runs a live,
// narratable "try to break it" pass against the running stack's money +
// attribution beacon paths. It fires REAL forgery / tamper / spoof / fraud /
// input attacks and prints whether each was rejected.
//
// It is deliberately fill-INDEPENDENT: rather than serve an ad (whose fill
// depends on advertiser budget state), it constructs a genuinely-signed
// impression beacon with the real signing code (pkg/adserving.SignURL + the dev
// key), so it works the same whether or not budgets are depleted. Every attack
// is real — "SECURE" means the platform actually rejected/neutralised it.
//
// The demonstrable subset lives here; the FULL 9-attack automated proof (incl.
// cross-advertiser conversion forgery, exactly-once dedup, cross-tenant reads)
// is `make test-e2e-security`.
//
//	make security-demo   # needs `make demo-forward` running
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ANSI colours, disabled when stdout isn't a terminal.
var tty = func() bool { fi, _ := os.Stdout.Stat(); return fi != nil && (fi.Mode()&os.ModeCharDevice) != 0 }()

func c(code, s string) string {
	if !tty {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

var (
	pass, fail int
	client     = &http.Client{Timeout: 8 * time.Second}
)

// fire GETs a beacon with a browser-shaped identity (so the FRAUD gate doesn't
// bot-block us — that isolates the SIGNATURE gate) and returns status + headers.
func fire(rawURL, ua string) (int, http.Header) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", "https://soundwave.adtech.local/")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

const realUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36"

func row(label string, ok bool, detail string) {
	if ok {
		fmt.Printf("  %s  %-38s %s\n", c("32", "🟢 SECURE"), label, c("2", detail))
		pass++
	} else {
		fmt.Printf("  %s  %-38s %s\n", c("31", "🔴 FAIL  "), label, c("2", detail))
		fail++
	}
}

func main() {
	gw := env("DEMO_GATEWAY", "http://localhost:8080")
	pubad := env("DEMO_PUBAD", "http://localhost:8088")

	fmt.Println()
	fmt.Println(c("1", "🔒 Live security demo — trying to break the money/attribution paths"))
	if code, _ := fire(gw+"/healthz", realUA); code != 200 {
		fmt.Println(c("31", "✗ can't reach "+gw+" — run 'make demo-forward' first"))
		os.Exit(1)
	}

	// Build a genuinely-signed impression beacon with the REAL signing code — no
	// ad-serve needed, so this is immune to budget/fill state.
	tid := "secdemo-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	q := url.Values{}
	q.Set("tid", tid)
	q.Set("advid", "b242356e-dfb0-5fe7-a52b-5210c9bc07a5")
	q.Set("cid", "4771be66-01fc-58cb-a895-8e2367115e90")
	q.Set("crid", "59c3bb4c-4bdc-58e8-bd99-96a4e69d8ae5")
	q.Set("pid", "e1469388-cebf-51ac-bce1-9d32d6c86bbc")   // SoundWave audio placement
	q.Set("pubid", "67bcab99-e110-58e6-ae29-4c6d46cfeaf9") // SoundWave publisher
	q.Set("bm", "cpm")
	q.Set("ch", "audio")
	q.Set("cur", "USD")
	q.Set("dev", "mobile")
	q.Set("geo", "USA")
	q.Set("h", "0")
	q.Set("w", "0")
	q.Set("price", "2.5000")
	q.Set("exp", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	signed := adserving.SignURL(gw+"/v1/t/imp?"+q.Encode(), adserving.DefaultSigningKey)
	fmt.Println(c("2", "  constructed a real HMAC-signed beacon (price=2.50, pubid=SoundWave) — now attacking it"))
	fmt.Println()

	// 0) Baseline: the untampered signed beacon is ACCEPTED — so a PASS below
	//    means the gate rejects tampering specifically, not all traffic.
	code, _ := fire(signed, realUA)
	row("valid signed beacon → accepted", code == 200, fmt.Sprintf("HTTP %d (expected 200)", code))

	// 1) Unsigned beacon (drop sig) → 403
	unsigned := stripParam(signed, "sig")
	code, _ = fire(unsigned, realUA)
	row("unsigned beacon rejected", code == 403, fmt.Sprintf("HTTP %d (expected 403)", code))

	// 2) Tampered price ×10, keep the old sig → 403 (price is inside the HMAC)
	tampered := replaceParam(signed, "price", "25.0000")
	code, _ = fire(tampered, realUA)
	row("tampered price (×10) rejected", code == 403, fmt.Sprintf("HTTP %d — price is inside the HMAC", code))

	// 3) Spoofed publisher (redirect revenue), keep the old sig → 403
	spoofed := replaceParam(signed, "pubid", "00000000-0000-0000-0000-000000000000")
	code, _ = fire(spoofed, realUA)
	row("spoofed publisher rejected", code == 403, fmt.Sprintf("HTTP %d — pubid is inside the HMAC", code))

	// 4) Bot traffic: fire the VALID beacon with a bot UA → fraud-blocked
	//    (200 pixel, but X-Dev-Fraud-Blocked:1, no row recorded).
	_, hdr := fire(signed, "curl/8.0")
	blocked := hdr != nil && hdr.Get("X-Dev-Fraud-Blocked") == "1"
	row("bot user-agent fraud-blocked", blocked, "X-Dev-Fraud-Blocked header set")

	// 5) Input safety: a malformed placement id must be a clean 4xx, never a 500.
	code, _ = fire(pubad+"/v1/pubad/serve?placement_id=not-a-uuid&format=display&geo=USA&device=desktop", realUA)
	row("malformed placement → 4xx not 500", code >= 400 && code < 500, fmt.Sprintf("HTTP %d (client error, no stack trace)", code))

	fmt.Println()
	if fail == 0 {
		fmt.Printf("  %s\n", c("1;32", fmt.Sprintf("%d/%d attacks rejected — money & attribution paths held.", pass, pass)))
	} else {
		fmt.Printf("  %s\n", c("1;31", fmt.Sprintf("%d attack(s) got through — investigate.", fail)))
	}
	fmt.Println(c("2", "  Full 9-attack automated proof (conversion forgery, exactly-once dedup,"))
	fmt.Println(c("2", "  cross-tenant reads):  make test-e2e-security"))
	fmt.Println()
	if fail != 0 {
		os.Exit(1)
	}
}

// stripParam removes a query parameter entirely.
func stripParam(raw, key string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Del(key)
	u.RawQuery = q.Encode()
	return u.String()
}

// replaceParam changes a param's value WITHOUT re-signing (the whole point: the
// stale sig must no longer match).
func replaceParam(raw, key, val string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// Rebuild the query preserving the original sig, swapping only `key`.
	q := u.Query()
	q.Set(key, val)
	u.RawQuery = q.Encode()
	return u.String()
}
