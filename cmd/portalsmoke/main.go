// portalsmoke is a host-run browser smoke for the ADVERTISER PORTAL — the
// UI layer no Go unit test can see. Born from two bugs that shipped silently
// and were only caught by a human clicking:
//
//  1. html/template's JS-context autoescape turned every component-rendered
//     onclick into a dead string literal ("+ Upload audience" did nothing).
//  2. Audience uploads binding by NAME let a typo strand members in a new,
//     unwired segment.
//
// What it drives (real Chrome, real gateway, real Postgres underneath):
//
//   - login via the actual form
//   - DEAD-ONCLICK REGRESSION: clicks the component-rendered "+ Upload
//     audience" button and asserts the modal actually opens
//   - upload-append semantics on a THROWAWAY audience (paste-ids path):
//     create with 3 ids → re-submit same name with 1 more → same segment id,
//     4 members, and the list shows the 'unwired' badge for it
//   - campaign page: openEdit() routes to #campaign/<id>, the section is
//     visible, and the segment picker lists audiences by name
//
// Self-contained: it creates its own uniquely-named audience and never
// touches demo data. Needs the stack up + seeded (adv-acme login).
//
// Usage: make test-portal   (or: go run ./cmd/portalsmoke -headless=false)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func main() {
	base := flag.String("base", "http://localhost:8080", "gateway base URL")
	email := flag.String("email", "adv-acme@adtech.local", "portal login")
	password := flag.String("password", "admin", "portal password")
	headless := flag.Bool("headless", true, "run Chrome headless")
	flag.Parse()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", *headless), chromedp.NoSandbox, chromedp.WindowSize(1400, 950),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()
	ctx, cancelTimeout := context.WithTimeout(ctx, 150*time.Second)
	defer cancelTimeout()

	fail := func(step string, err error) {
		log.Printf("FAIL %s: %v", step, err)
		os.Exit(1)
	}
	nonce := fmt.Sprintf("smoke-%d", time.Now().UnixNano())

	// -- login + portal boot ------------------------------------------------
	if err := chromedp.Run(ctx,
		chromedp.Navigate(*base+"/login"),
		chromedp.WaitVisible(`input[name=email],#email`, chromedp.ByQuery),
		chromedp.SendKeys(`input[name=email],#email`, *email, chromedp.ByQuery),
		chromedp.SendKeys(`input[name=password],#password`, *password, chromedp.ByQuery),
		chromedp.Submit(`input[name=password],#password`, chromedp.ByQuery),
		chromedp.Sleep(1500*time.Millisecond),
		chromedp.Navigate(*base+"/portal/advertiser#audiences"),
		chromedp.Sleep(1800*time.Millisecond),
	); err != nil {
		fail("login", err)
	}
	log.Println("ok: login + portal boot")

	// -- 1. dead-onclick regression: the REAL button must open the modal ----
	var modalHidden bool
	if err := chromedp.Run(ctx,
		chromedp.Click(`//button[contains(., '+ Upload audience')]`, chromedp.BySearch),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Evaluate(`document.getElementById('newAudience').classList.contains('hidden')`, &modalHidden),
	); err != nil {
		fail("upload-button click", err)
	}
	if modalHidden {
		fail("dead-onclick regression", fmt.Errorf("clicking '+ Upload audience' did not open the modal (html/template onclick escaping?)"))
	}
	log.Println("ok: component onclick opens the upload modal")

	// -- 2. create a throwaway audience via paste-ids, then append ----------
	upload := func(ids string) (segID string, members int, err error) {
		var out map[string]any
		script := fmt.Sprintf(`(async () => {
			const r = await fetch('/v1/api/audiences', {method:'POST', headers:{'Content-Type':'application/json'},
				body: JSON.stringify({name: %q, visibility: 'dsp_private', user_ids: %s})});
			const j = await r.json();
			const list = await (await fetch('/v1/api/audiences')).json();
			const seg = (list || []).find(a => a.name === %q) || {};
			return {segment_id: j.segment_id || '', members: seg.members || 0, unwired: !((seg.targeted_by||[]).length)};
		})()`, nonce, ids, nonce)
		if err := chromedp.Run(ctx, chromedp.Evaluate(script, &out,
			func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) })); err != nil {
			return "", 0, err
		}
		id, _ := out["segment_id"].(string)
		m, _ := out["members"].(float64)
		if unwired, _ := out["unwired"].(bool); !unwired {
			return "", 0, fmt.Errorf("fresh smoke audience reported as targeted — targeted_by wiring broken")
		}
		return id, int(m), nil
	}
	seg1, m1, err := upload(`['pm-u1','pm-u2','pm-u3']`)
	if err != nil || seg1 == "" || m1 != 3 {
		fail("audience create", fmt.Errorf("seg=%q members=%d err=%v", seg1, m1, err))
	}
	seg2, m2, err := upload(`['pm-u1','pm-u4']`)
	if err != nil || seg2 != seg1 || m2 != 4 {
		fail("audience append", fmt.Errorf("want same segment %s w/ 4 members; got %s w/ %d (err=%v)", seg1, seg2, m2, err))
	}
	log.Println("ok: upload-append semantics (same segment id, 3→4 members, unwired badge data)")

	// -- 3. campaign page routes + picker lists audiences -------------------
	var hash, pickText string
	var pageVisible bool
	if err := chromedp.Run(ctx,
		chromedp.Evaluate(`document.getElementById('newAudience').classList.add('hidden'); location.hash='#campaigns';`, nil),
		chromedp.Sleep(1200*time.Millisecond),
		chromedp.Evaluate(`openEdit((campaignsCache[0]||{}).ID || '')`, nil),
		chromedp.Sleep(1800*time.Millisecond),
		chromedp.Evaluate(`location.hash`, &hash),
		chromedp.Evaluate(`!document.getElementById('section-campaign').classList.contains('hidden')`, &pageVisible),
		chromedp.Evaluate(`document.getElementById('ec_seg_inc_pick').textContent`, &pickText),
	); err != nil {
		fail("campaign page", err)
	}
	if !strings.HasPrefix(hash, "#campaign/") || !pageVisible {
		fail("campaign page routing", fmt.Errorf("hash=%q visible=%v", hash, pageVisible))
	}
	if !strings.Contains(pickText, nonce) {
		fail("segment picker", fmt.Errorf("picker does not list the smoke audience %q (got: %.120s)", nonce, pickText))
	}
	log.Printf("ok: #campaign/<id> page + segment picker lists %q", nonce)

	log.Println("PORTALSMOKE PASS")
}
