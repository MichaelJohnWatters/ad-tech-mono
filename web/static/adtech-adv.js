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
 *     adtechadv.conversion('purchase', 42999);    // on checkout -> billed conversion
 *   </script>
 */
(function () {
    'use strict';

    var config = { trackerUrl: '', accountId: '', tag: '', debug: false };
    var state = { consented: false, uid: null };

    function pixel(url) { var i = new Image(1, 1); i.src = url; }
    function log() {
        if (config.debug && window.console) {
            console.log.apply(console, ['[adtechadv]'].concat([].slice.call(arguments)));
        }
    }

    // First-party visitor id the advertiser owns (a hashed id, persisted). Real
    // advertisers key this on a hashed email / CRM id; here we synthesise one
    // once and persist it, exactly as a first-party site would.
    function visitorId() {
        var id = localStorage.getItem('adtechadv_uid');
        if (id) return Promise.resolve(id);
        return crypto.subtle.digest('SHA-256', new TextEncoder().encode('visitor-' + Math.random() + Date.now()))
            .then(function (buf) {
                id = 'he_' + Array.from(new Uint8Array(buf)).slice(0, 16)
                    .map(function (b) { return b.toString(16).padStart(2, '0'); }).join('');
                localStorage.setItem('adtechadv_uid', id);
                return id;
            });
    }

    var adtechadv = {
        init: function (opts) {
            config.trackerUrl = (opts.trackerUrl || '').replace(/\/$/, '');
            config.accountId = opts.accountId || '';
            config.tag = opts.tag || '';
            config.debug = !!opts.debug;
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
                pixel(config.trackerUrl + '/v1/t/rt?uid=' + encodeURIComponent(uid) +
                    '&aid=' + encodeURIComponent(config.accountId) +
                    '&tag=' + encodeURIComponent(config.tag) +
                    '&tid=rt-' + Date.now());
                log('retargeting pixel fired', config.tag);
                return uid;
            });
        },

        // conversion(type, rev) fires a billed conversion event (consent-gated).
        // Returns false and fires nothing without consent.
        conversion: function (type, rev) {
            if (!state.consented) { log('conversion suppressed — no consent'); return false; }
            pixel(config.trackerUrl + '/v1/t/conv?tid=order-' + Date.now() +
                '&type=' + encodeURIComponent(type) + '&rev=' + (rev || 0) + '&cur=USD');
            log('conversion pixel fired', type, rev);
            return true;
        },

        getVisitorId: function () { return state.uid; },
        hasConsent: function () { return state.consented; }
    };

    window.adtechadv = adtechadv;
})();
