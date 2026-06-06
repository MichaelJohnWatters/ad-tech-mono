// Themed creative HTML for the pub sim. Replaces the previous flat
// gray placeholder so the sim looks like a real platform you could
// demo to someone. Six themes (retail, tech, auto, finance, gaming,
// privacy) picked by domain substring — keeps the renderer dispatch
// data-driven instead of forcing every creative YAML to specify a
// theme name.
//
// All templates use ${WIDTH}/${HEIGHT} so the ad server's macro
// substitution can render the same template at any standard IAB
// size (300×250 MPU, 728×90 leaderboard, 300×600 half-page, etc.).
// CTAs wrap a ${CLICK_URL} anchor so the tracker click pixel fires
// before the redirect lands.
package main

import (
	"strings"
)

// themedCreativeHTML returns inline HTML for the (externalID, domain,
// landingURL) tuple. Theme is picked by matching the domain against
// a substring table. Unknown domains fall back to the generic
// "default" theme (a cleaner version of the old placeholder).
func themedCreativeHTML(externalID, domain string) string {
	if domain == "" {
		domain = "example.com"
	}
	landing := "https://" + domain
	theme := themeForDomain(domain)
	return theme(externalID, domain, landing)
}

// themeForDomain returns the renderer for the matched theme. Order
// matters — first hit wins. Substring match (so "luxauto" catches
// any subdomain) and case-insensitive (defensive — seed YAMLs vary).
func themeForDomain(domain string) func(externalID, domain, landingURL string) string {
	d := strings.ToLower(domain)
	switch {
	case containsAny(d, "shoes", "store", "shop", "bite", "retail"):
		return renderRetail
	case containsAny(d, "tech", "saas", "cloud", "soft", "crm", "init"):
		return renderTech
	case containsAny(d, "auto", "motors", "drive"):
		return renderAuto
	case containsAny(d, "crypto", "finance", "bank", "trade", "invest"):
		return renderFinance
	case containsAny(d, "game", "quest", "play"):
		return renderGaming
	case containsAny(d, "vpn", "secure", "privacy", "shield"):
		return renderPrivacy
	default:
		return renderDefault
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// renderRetail — warm coral/peach with a shopping bag emoji.
// E-commerce / fashion / food delivery.
func renderRetail(externalID, domain, landingURL string) string {
	brand := brandFromDomain(domain)
	return wrapTemplate(landingURL,
		`background:linear-gradient(135deg,#ff7043,#ff5252);color:#fff;text-shadow:0 1px 2px rgba(0,0,0,0.2);`,
		`<div style="font-size:42px;line-height:1;">🛍️</div>
		 <div style="font-weight:700;font-size:20px;margin-top:6px;">`+brand+`</div>
		 <div style="font-size:12px;opacity:0.95;margin-top:2px;">Up to 40% off this week</div>`,
		`Shop Now`, `#fff`, `#ff5252`)
}

// renderTech — blue gradient + lightning. SaaS / cloud / dev tools.
func renderTech(externalID, domain, landingURL string) string {
	brand := brandFromDomain(domain)
	return wrapTemplate(landingURL,
		`background:linear-gradient(135deg,#4361ee,#3a0ca3);color:#fff;`,
		`<div style="font-size:42px;line-height:1;">⚡</div>
		 <div style="font-weight:700;font-size:20px;margin-top:6px;">`+brand+`</div>
		 <div style="font-size:12px;opacity:0.9;margin-top:2px;">Built for engineering teams</div>`,
		`Start Free Trial`, `#fff`, `#3a0ca3`)
}

// renderAuto — dark gradient + car emoji. Automotive / luxury.
func renderAuto(externalID, domain, landingURL string) string {
	brand := brandFromDomain(domain)
	return wrapTemplate(landingURL,
		`background:linear-gradient(135deg,#1a1a2e,#2c2c54);color:#fff;`,
		`<div style="font-size:42px;line-height:1;">🚗</div>
		 <div style="font-weight:700;font-size:20px;margin-top:6px;letter-spacing:0.5px;">`+brand+`</div>
		 <div style="font-size:12px;opacity:0.85;margin-top:2px;">The new model has arrived</div>`,
		`Schedule Test Drive`, `#1a1a2e`, `#f1c40f`)
}

// renderFinance — green/dark with a chart emoji.
// Crypto / brokerage / finance.
func renderFinance(externalID, domain, landingURL string) string {
	brand := brandFromDomain(domain)
	return wrapTemplate(landingURL,
		`background:linear-gradient(135deg,#0d2818,#1d3a1f);color:#fff;`,
		`<div style="font-size:42px;line-height:1;">📈</div>
		 <div style="font-weight:700;font-size:20px;margin-top:6px;">`+brand+`</div>
		 <div style="font-size:12px;opacity:0.85;margin-top:2px;">Low fees, 24/7 markets</div>`,
		`Trade Today`, `#0d2818`, `#2ecc71`)
}

// renderGaming — purple/neon + controller.
func renderGaming(externalID, domain, landingURL string) string {
	brand := brandFromDomain(domain)
	return wrapTemplate(landingURL,
		`background:linear-gradient(135deg,#240046,#5a189a);color:#fff;`,
		`<div style="font-size:42px;line-height:1;">🎮</div>
		 <div style="font-weight:700;font-size:20px;margin-top:6px;">`+brand+`</div>
		 <div style="font-size:12px;opacity:0.85;margin-top:2px;">Free to play. No download.</div>`,
		`Play Now`, `#240046`, `#ffd60a`)
}

// renderPrivacy — orange/dark with a shield emoji. VPN / security.
func renderPrivacy(externalID, domain, landingURL string) string {
	brand := brandFromDomain(domain)
	return wrapTemplate(landingURL,
		`background:linear-gradient(135deg,#2d1b00,#503000);color:#fff;`,
		`<div style="font-size:42px;line-height:1;">🛡️</div>
		 <div style="font-weight:700;font-size:20px;margin-top:6px;">`+brand+`</div>
		 <div style="font-size:12px;opacity:0.9;margin-top:2px;">Block trackers. Encrypt traffic.</div>`,
		`Get Protected`, `#2d1b00`, `#ff9f1c`)
}

// renderDefault — clean blue card. Used when domain doesn't match
// any other theme. Better-looking than the previous flat gray.
func renderDefault(externalID, domain, landingURL string) string {
	brand := brandFromDomain(domain)
	return wrapTemplate(landingURL,
		`background:linear-gradient(135deg,#4cc9f0,#4361ee);color:#fff;`,
		`<div style="font-size:42px;line-height:1;">✨</div>
		 <div style="font-weight:700;font-size:20px;margin-top:6px;">`+brand+`</div>
		 <div style="font-size:12px;opacity:0.9;margin-top:2px;">`+domain+`</div>`,
		`Visit`, `#4361ee`, `#fff`)
}

// wrapTemplate returns the full 300×250 banner HTML (responsive to
// ${WIDTH}/${HEIGHT} macros). innerHTML contains the icon + headline
// + tagline; ctaLabel + ctaBg + ctaFg style the click button.
// The whole creative is wrapped in an anchor pointing at ${CLICK_URL}
// — the tracker URL with redir=landing baked in by the ad server's
// macro substitution. Tracker fires the click event, then 302s to the
// advertiser page. Impression pixel sits at the end as a hidden 1×1.
func wrapTemplate(_landingURL, surfaceCSS, innerHTML, ctaLabel, ctaBg, ctaFg string) string {
	return `<a href="${CLICK_URL}" target="_blank" rel="noopener" style="text-decoration:none;color:inherit;display:block;">` +
		`<div style="width:${WIDTH}px;height:${HEIGHT}px;` + surfaceCSS +
		`box-sizing:border-box;padding:18px;display:flex;flex-direction:column;align-items:center;justify-content:center;` +
		`font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;text-align:center;border-radius:4px;overflow:hidden;position:relative;cursor:pointer;">` +
		innerHTML +
		`<div style="background:` + ctaBg + `;color:` + ctaFg + `;padding:8px 22px;border-radius:999px;font-size:13px;font-weight:600;margin-top:12px;letter-spacing:0.3px;">` + ctaLabel + `</div>` +
		`<img src="${IMP_PIXEL}" width="1" height="1" style="position:absolute;left:0;top:0;opacity:0;" alt="" />` +
		`</div></a>`
}

// brandFromDomain takes "acme-shoes.com" and returns "ACME". Drops
// the TLD + everything after a dash so multi-word brands ("luxauto")
// show as "LUXAUTO" and dashed brands ("acme-shoes") show as "ACME".
func brandFromDomain(domain string) string {
	base := strings.SplitN(domain, ".", 2)[0]
	if i := strings.Index(base, "-"); i > 0 {
		base = base[:i]
	}
	return strings.ToUpper(base)
}
