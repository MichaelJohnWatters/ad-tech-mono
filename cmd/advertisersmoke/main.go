// cmd/advertisersmoke drives a REAL headless Chrome against the demo ADVERTISER
// site ("Ford") and exercises the shared buy-side tag (adtech-adv.js): it gives
// consent (which fires the retargeting pixel) and triggers a conversion (which
// fires the conversion pixel). The downstream assertion — that a site_visit
// behaviour signal and a conversion row land in ClickHouse — is done by
// scripts/advertiser-smoke.sh around this run, proving the whole chain
// browser -> adtech-adv.js -> tracker -> NATS -> reporting -> ClickHouse.
//
// The buy-side twin of cmd/viewabilitysmoke. Opt-in (make advertiser-smoke);
// needs a real Chrome + a running demoadv + the live stack.
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
	url := flag.String("url", "http://localhost:9200/models/f150", "demo advertiser product page (embeds adtech-adv.js)")
	headless := flag.Bool("headless", true, "run Chrome headless")
	convType := flag.String("type", "purchase", "conversion type to fire")
	rev := flag.String("rev", "42999", "conversion revenue")
	flag.Parse()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", *headless),
		chromedp.NoSandbox,
		chromedp.WindowSize(1280, 900),
		// Real Chrome UA — the default headless UA is fraud-flagged as a bot and
		// the tracker drops the pixel (see cmd/viewabilitysmoke).
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	ctx, cancelCtx := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer cancelCtx()
	ctx, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	defer cancelTimeout()

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

	log.Printf("advertiser-smoke: driving %s (headless=%v)", *url, *headless)
	err := chromedp.Run(ctx,
		chromedp.Navigate(*url),
		// Wait for the shared advertiser tag + the page's convert() helper to load.
		chromedp.Poll(`typeof window.adtechadv !== 'undefined' && typeof convert === 'function'`, nil, chromedp.WithPollingTimeout(20*time.Second)),
		// Consent → the tag fires the retargeting pixel (site_visit) + sets the uid cookie.
		chromedp.Evaluate(`applyConsent(true)`, nil),
		chromedp.Sleep(1500*time.Millisecond),
		// Convert → the page POSTs the sale to the advertiser's own /convert, which
		// signs + fires the conversion server-to-server.
		chromedp.Evaluate(`convert('`+*convType+`', `+*rev+`)`, nil),
		chromedp.Sleep(3*time.Second),
	)
	if err != nil {
		log.Fatalf("advertiser-smoke: chrome drive failed: %v", err)
	}
	log.Printf("advertiser-smoke: consented (retargeting pixel) + posted conversion (server-to-server, signed)")
}
