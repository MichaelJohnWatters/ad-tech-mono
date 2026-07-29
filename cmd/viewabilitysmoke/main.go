// cmd/viewabilitysmoke drives a REAL headless Chrome against the demo publisher's
// video page and lets the page's own IntersectionObserver self-measure IAB video
// viewability and fire the signed viewability beacon — the one client-side step
// the HTTP-only e2e harness can't exercise (it has no browser DOM).
//
// It only DRIVES the browser (navigate → consent → play → let the observer dwell
// and fire). The downstream assertion — that a channel=video, iab_viewable=1 row
// landed in ClickHouse — is done by scripts/viewability-smoke.sh around this run,
// so the whole chain browser → tracker → NATS → reporting → ClickHouse is proven.
//
// Opt-in only (make viewability-smoke); never part of `make test-e2e`, because it
// needs a real Chrome + a running demosite + the live stack.
package main

import (
	"context"
	"flag"
	"log"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func main() {
	url := flag.String("url", "http://localhost:9000/p/video-hub", "demosite page carrying a self-measuring video ad")
	headless := flag.Bool("headless", true, "run Chrome headless (set false to watch it)")
	dwell := flag.Duration("dwell", 8*time.Second, "time to let the video play + the viewability observer reach its 2s dwell and fire")
	flag.Parse()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", *headless),
		// Let the muted <video> autoplay without a user gesture, exactly as a
		// real page's auto-starting player would.
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		chromedp.Flag("mute-audio", true),
		chromedp.NoSandbox,
		chromedp.WindowSize(1280, 900),
		// Real Chrome UA — the default headless UA contains "HeadlessChrome",
		// which the tracker's real-time fraud check flags as a bot and drops the
		// beacon (so nothing would reach ClickHouse). A real visitor isn't headless.
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	ctx, cancelCtx := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer cancelCtx()
	ctx, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	defer cancelTimeout()

	// Surface page console + errors so a failure is diagnosable.
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch e := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			var parts []string
			for _, a := range e.Args {
				parts = append(parts, string(a.Value))
			}
			log.Printf("  [console.%s] %s", e.Type, strings.Join(parts, " "))
		case *runtime.EventExceptionThrown:
			log.Printf("  [page error] %s", e.ExceptionDetails.Text)
		}
	})

	log.Printf("viewability-smoke: driving %s (headless=%v)", *url, *headless)
	var playState float64
	err := chromedp.Run(ctx,
		chromedp.Navigate(*url),
		// Wait for the async-loaded adtech.js SDK to be present BEFORE consenting —
		// applyConsent() calls into it, so consenting too early would throw and the
		// page would never request ads.
		chromedp.Poll(`typeof window.adtech !== 'undefined' && typeof window.adtech.requestVideoAd === 'function'`, nil, chromedp.WithPollingTimeout(20*time.Second)),
		// Give consent so the page requests its ads (the banner gates ad loading).
		chromedp.Evaluate(`applyConsent(true)`, nil),
		// The <video> is created only after the VAST fills — wait for it.
		chromedp.WaitReady(`video`, chromedp.ByQuery),
		// Scroll it fully on-screen so the IntersectionObserver sees >=50% and the
		// dwell clock starts, then make sure it's actually playing.
		chromedp.ScrollIntoView(`video`, chromedp.ByQuery),
		chromedp.Evaluate(`(function(){ var v=document.querySelector('video'); if(!v) return -1; v.muted=true; var p=v.play(); if(p&&p.catch)p.catch(function(){}); return v.readyState; })()`, &playState),
		// Let it play through the observer's 2s continuous-visibility dwell + margin.
		chromedp.Sleep(*dwell),
	)
	if err != nil {
		// Dump the video slot's DOM to help diagnose a no-fill vs a play failure.
		var slot string
		_ = chromedp.Run(ctx, chromedp.Evaluate(`(document.querySelector('.slot-box')||{}).outerHTML || '(no slot)'`, &slot))
		log.Printf("viewability-smoke: slot state on failure: %.400s", slot)
		log.Fatalf("viewability-smoke: chrome drive failed: %v", err)
	}
	log.Printf("viewability-smoke: video present (readyState=%.0f), played through the dwell window — the page's IntersectionObserver has had time to fire the viewability beacon", playState)
}
