package adserving

import "encoding/json"

// WrapExternalDisplayHTML wraps an EXTERNAL buyer's display HTML with the
// platform's beacons: an outer div (so script-/iframe-shaped markup still
// renders normally, with our beacons outside the buyer's DOM scope), a 1×1
// impression <img> that fires on render, and a self-contained viewability
// observer. Used by both external-display serve paths — the publisher-
// adserver's Prebid fan-out and the SSP's exchange-path adm consumption —
// so a buyer's creative renders verbatim while OUR signed beacons keep the
// zero-data-slippage contract. wrapperAttr labels the wrapper div
// ("data-prebid-wrapper" / "data-external-adm-wrapper") so pages and tests
// can tell the paths apart.
//
// The buyer's own pixels (if any) fire additively — billing counts only the
// platform's signed impression. Auction macros (${AUCTION_PRICE} …) must be
// substituted by the CALLER before wrapping (OpenRTB §4.4: the exchange does
// it for exchange winners; the Prebid path expands locally).
func WrapExternalDisplayHTML(buyerHTML, impressionURL, viewabilityURL, wrapperAttr string) string {
	return `<div ` + wrapperAttr + `="1" style="display:block;width:100%;height:100%;">` +
		buyerHTML +
		`<img src="` + impressionURL + `" width="1" height="1" style="display:none;" alt="" />` +
		ViewabilityBeaconScript(viewabilityURL) +
		`</div>`
}

// ViewabilityBeaconScript returns an inline <script> that observes its own
// wrapper element and fires the signed viewability URL once the IAB display
// threshold is met (≥50% of pixels on-screen for ≥1 continuous second),
// appending the client-measured dur/pct/area (the tracker excludes those
// from signature validation — see cmd/tracker viewSigParams). A faithful,
// no-SDK port of web/static/adtech.js observeViewability, so an external
// render self-reports viewability without loading our SDK. Uses an Image()
// GET (no CORS preflight, fires cross-origin) and document.currentScript to
// scope to its own ad when several render on one page.
func ViewabilityBeaconScript(viewabilityURL string) string {
	u, _ := json.Marshal(viewabilityURL) // safe JS string literal
	return `<script>(function(){` +
		`if(!('IntersectionObserver' in window))return;` +
		`var s=document.currentScript,w=s&&s.parentElement;if(!w)return;` +
		`var u=` + string(u) + `,t=null,done=false;` +
		`var o=new IntersectionObserver(function(es){var e=es[0];` +
		`if(e.isIntersecting&&e.intersectionRatio>=0.5){if(!t)t=Date.now();}else{t=null;}` +
		`if(!done&&t&&(Date.now()-t)>=1000){done=true;` +
		`var d=Date.now()-t,p=Math.round(e.intersectionRatio*100),a=w.offsetWidth*w.offsetHeight;` +
		`var sep=u.indexOf('?')===-1?'?':'&';` +
		`(new Image()).src=u+sep+'dur='+d+'&pct='+p+'&area='+a;o.disconnect();}` +
		`},{threshold:[0,0.5,1.0]});o.observe(w);})();</script>`
}
