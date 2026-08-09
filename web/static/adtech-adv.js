/*!
 * adtech-adv.js — the ad-tech platform's ADVERTISER-side tag.
 *
 * The buy-side counterpart to the publisher SDK (adtech.js). Advertisers embed
 * this on their OWN site to (a) build retargeting audiences from consented
 * visits and (b) report conversions. Everything fires as a 1x1 pixel to the
 * platform tracker; the visitor id is a first-party, advertiser-owned hashed id.
 *
 *   <script src="https://cdn.adtech.example/adtech-adv.js"></script>
 *   <script>
 *     adtechadv.init({ trackerUrl: 'https://tracker.adtech.example', accountId: 'adv-123', tag: 'truck-pdp' });
 *     adtechadv.setConsent(true);                 // consented visit -> retargeting pixel
 *   </script>
 *
 * Conversions are NOT fired from the browser (they drive CPA billing, so the
 * conversion URL is HMAC-signed and posted server-to-server by the advertiser's
 * backend — see the NOTE below and cmd/demoadv). This tag captures the earning
 * click trace off the landing URL so that server postback can attribute it.
 */
(function () {
    'use strict';

    var config = { trackerUrl: '', accountId: '', tag: '', debug: false };
    var state = { consented: false, uid: null, he: null, ip: null };

    // sha256Hex hashes the normalised (trimmed, lower-cased) value to lowercase
    // hex — the standard hashed-email form advertisers and publishers share, so
    // the same person hashes to the same id on both sides (the bridge key).
    function sha256Hex(value) {
        return crypto.subtle.digest('SHA-256', new TextEncoder().encode(String(value).trim().toLowerCase()))
            .then(function (buf) {
                return Array.from(new Uint8Array(buf))
                    .map(function (b) { return b.toString(16).padStart(2, '0'); }).join('');
            });
    }

    function pixel(url) { var i = new Image(1, 1); i.src = url; }
    function log() {
        if (config.debug && window.console) {
            console.log.apply(console, ['[adtechadv]'].concat([].slice.call(arguments)));
        }
    }

    // First-party visitor id the advertiser owns (a hashed id, persisted). Real
    // advertisers key this on a hashed email / CRM id; here we synthesise one
    // once and persist it, exactly as a first-party site would. It's persisted in
    // BOTH localStorage and a first-party cookie so the advertiser's OWN SERVER
    // can read it to attribute a server-to-server conversion (see below).
    function visitorId() {
        var id = localStorage.getItem('adtechadv_uid');
        if (id) { setCookie('adtechadv_uid', id); return Promise.resolve(id); }
        return crypto.subtle.digest('SHA-256', new TextEncoder().encode('visitor-' + Math.random() + Date.now()))
            .then(function (buf) {
                id = 'he_' + Array.from(new Uint8Array(buf)).slice(0, 16)
                    .map(function (b) { return b.toString(16).padStart(2, '0'); }).join('');
                localStorage.setItem('adtechadv_uid', id);
                setCookie('adtechadv_uid', id);
                return id;
            });
    }
    function setCookie(name, value, days) {
        var d = new Date(); d.setTime(d.getTime() + (days || 90) * 86400000);
        document.cookie = name + '=' + value + ';path=/;expires=' + d.toUTCString() + ';SameSite=Lax';
    }
    function getQueryParam(name) {
        var m = new RegExp('[?&]' + name + '=([^&#]*)').exec(window.location.search);
        return m ? decodeURIComponent(m[1]) : null;
    }

    // captureClickTrace closes the attribution loop's first half. When a user
    // arrives via an ad click, the platform tracker's 302 stamps the earning
    // exposure's trace onto the landing URL as ?adtech_tid=... (the gclid
    // analog). We persist it first-party (localStorage + cookie, click-window
    // TTL) so the advertiser's OWN SERVER can read it and return it on the
    // signed conversion postback — which is what lets the platform settle CPA
    // against the impression that actually earned the conversion.
    function captureClickTrace() {
        var t = getQueryParam('adtech_tid');
        if (t) {
            localStorage.setItem('adtech_ctid', t);
            setCookie('adtech_ctid', t, 30); // = default click-through window
            log('captured click trace', t.slice(0, 12) + '…');
        }
        return localStorage.getItem('adtech_ctid');
    }

    var adtechadv = {
        init: function (opts) {
            config.trackerUrl = (opts.trackerUrl || '').replace(/\/$/, '');
            config.accountId = opts.accountId || '';
            config.tag = opts.tag || '';
            config.debug = !!opts.debug;
            // Grab the click trace off the landing URL right away, before any
            // client-side routing rewrites the query string.
            captureClickTrace();
            state.he = localStorage.getItem('adtechadv_he') || null;
            log('init', config.accountId, 'tag=' + config.tag);
        },

        // setConsent(true) records consent AND fires the retargeting pixel — this
        // visit joins the advertiser's audience for config.tag (consent-gated at
        // the platform). Returns a promise of the visitor id. setConsent(false)
        // fires nothing.
        setConsent: function (personalized) {
            state.consented = !!personalized;
            if (!personalized) { log('consent declined — no retargeting'); return Promise.resolve(null); }
            return visitorId().then(function (uid) {
                state.uid = uid;
                // he (hashed email) is the shared id that lets the platform bridge
                // this advertiser visitor to the publisher-side user who saw the
                // ad — sent only when the advertiser has identified the visitor
                // (setEmail) and consent is given.
                var he = state.he ? '&he=' + encodeURIComponent(state.he) : '';
                // No tid: a site visit has no upstream auction trace to reference.
                // The tracker mints a real 32-hex trace for this visit server-side
                // (its HTTPMiddleware), so we never invent a client-side id that
                // would pollute the trace_id column with a non-standard format.
                // Demo/dev household override: rides the tracker's allowlist-
                // gated ?ip= (private callers only — inert from public browsers
                // in prod), so local demo personas get their own household and
                // ANONYMOUS guest carts are household-chaseable with no email.
                var ip = state.ip ? '&ip=' + encodeURIComponent(state.ip) : '';
                pixel(config.trackerUrl + '/v1/t/rt?uid=' + encodeURIComponent(uid) +
                    '&aid=' + encodeURIComponent(config.accountId) +
                    '&tag=' + encodeURIComponent(config.tag) + he + ip);
                log('retargeting pixel fired', config.tag + (state.he ? ' (+hashed email)' : ''));
                return uid;
            });
        },

        // setDemoIP sets the demo/dev shopper IP forwarded on the retargeting
        // pixel (?ip=). Local demos only — the tracker honours it solely from
        // allowlisted (private) callers, so a public browser setting this is
        // ignored server-side.
        setDemoIP: function (ip) { state.ip = ip || null; },

        // setEmail identifies the visitor by a hashed email (the advertiser knows
        // its logged-in customer). Stored first-party; included on the next
        // retargeting pixel so the platform can link this advertiser visitor to
        // the same person's publisher-side id. Returns a promise of the hash.
        setEmail: function (email) {
            if (!email) { return Promise.resolve(null); }
            return sha256Hex(email).then(function (he) {
                state.he = he;
                localStorage.setItem('adtechadv_he', he);
                log('visitor email hashed', he.slice(0, 12) + '…');
                return he;
            });
        },

        // NOTE: there is deliberately NO browser conversion() method. A conversion
        // is the platform's CPA BILLING trigger, so /v1/t/conv is HMAC-signed and
        // MUST be fired server-to-server by the advertiser's backend (which the
        // platform issues a signing key to) — never from the browser, where an
        // unsigned pixel would be a billing-fraud surface. The browser tag only
        // does audience/measurement work (setConsent -> retargeting) and exposes
        // the visitor id (localStorage + cookie) so the advertiser's server can
        // attribute its S2S conversion postback. See cmd/demoadv for the pattern.

        getVisitorId: function () { return state.uid; },
        hasConsent: function () { return state.consented; },
        // The captured earning-click trace (null if this visitor didn't arrive
        // via a tracked ad click). The advertiser's server reads the matching
        // first-party cookie to attach it to the S2S conversion postback.
        getClickTrace: function () { return localStorage.getItem('adtech_ctid'); }
    };

    window.adtechadv = adtechadv;
})();
