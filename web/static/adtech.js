/**
 * Ad Tech Platform - Publisher Ad Tag SDK
 *
 * Drop-in tag publishers embed on their pages. Calls our publisher-side
 * ad server (publisher-adserver / "pubad"), which arbitrates direct-sold
 * inventory, Prebid demand, and the SSP+exchange path server-side and
 * returns ready-to-render HTML plus pre-signed tracking URLs. This is the
 * GPT-equivalent in our stack: one tag, the publisher gets the whole
 * waterfall handled.
 *
 * Publishers running Prebid.js client-side use the separate
 * prebid-adtechmono-adapter.js module instead — they don't load this
 * tag (Prebid manages slot rendering itself).
 *
 * Usage:
 *   <script src="https://cdn.adtech.example/adtech.js"></script>
 *   <script>
 *     adtech.init({ publisherId: 'pub-123', siteId: 'site-456' });
 *     adtech.setConsent({ gdpr: true, purposes: [1,2,3,4] });
 *     adtech.setPageContext({ categories: ['IAB17'], keywords: ['football'] });
 *     adtech.requestAd({ placementId: 'pl-news-mpu', elementId: 'ad-slot-1' });
 *   </script>
 */
(function(window) {
    'use strict';

    var SDK_VERSION = '2.0.0';
    var COOKIE_NAME = 'adtech_uid';
    var COOKIE_DAYS = 90;
    // Server defaults to USA + UA-inferred device when these are blank;
    // we still send the platform ID so DSPs can frequency-cap + segment-
    // target without the SDK having to parse User-Agent itself.

    var config = {
        publisherId: null,
        siteId: null,
        pubadUrl: null,   // resolved in init() — base of /v1/pubad/serve
        debug: false
    };

    var state = {
        platformId: null,
        consent: { gdpr: false, purposes: [] },
        pageContext: { categories: [], keywords: [], contentRating: null },
        userData: {}
    };

    // ============================================================
    // Platform ID (first-party cookie)
    // ============================================================

    function getPlatformId() {
        var cookie = getCookie(COOKIE_NAME);
        if (cookie) return cookie;

        // Check consent before setting cookie
        if (state.consent.gdpr && !state.consent.purposes.includes(1)) {
            return null; // no consent for cookie storage
        }

        var id = 'pid-' + randomHex(8) + '-' + randomHex(4) + '-' + randomHex(4);
        setCookie(COOKIE_NAME, id, COOKIE_DAYS);
        return id;
    }

    function deletePlatformId() {
        setCookie(COOKIE_NAME, '', -1);
        state.platformId = null;
    }

    // ============================================================
    // Public API
    // ============================================================

    var adtech = {
        version: SDK_VERSION,

        /**
         * Initialise the SDK with publisher config.
         *
         * opts.pubadUrl overrides the publisher-adserver base URL.
         * When omitted the SDK assumes it's served from the same gateway
         * the publisher embedded the tag from (no CORS preflight cost).
         */
        init: function(opts) {
            opts = opts || {};
            config.publisherId = opts.publisherId || null;
            config.siteId = opts.siteId || null;
            config.pubadUrl = opts.pubadUrl || (window.location.protocol + '//' + window.location.host);
            config.debug = opts.debug || false;

            state.platformId = getPlatformId();

            if (config.debug) {
                console.log('[adtech] init', { config: config, platformId: state.platformId });
            }
        },

        /**
         * Set user consent status.
         */
        setConsent: function(consent) {
            state.consent = {
                gdpr: consent.gdpr || false,
                purposes: consent.purposes || []
            };

            // If consent withdrawn, delete platform ID
            if (consent.gdpr && !consent.purposes.includes(1)) {
                deletePlatformId();
            }

            if (config.debug) {
                console.log('[adtech] consent set', state.consent);
            }
        },

        /**
         * Set page-level contextual signals.
         */
        setPageContext: function(ctx) {
            state.pageContext = {
                categories: ctx.categories || [],
                keywords: ctx.keywords || [],
                contentRating: ctx.contentRating || null
            };
        },

        /**
         * Set first-party user data (hashed client-side before calling this).
         */
        setUserData: function(data) {
            state.userData = {
                hashedEmail: data.hashedEmail || null,
                segments: data.segments || [],
                age: data.age || null,
                gender: data.gender || null
            };
        },

        /**
         * Request and render an ad.
         *
         * opts.placementId — required, the platform placement external ID.
         * opts.elementId   — required, DOM id of the slot div.
         * opts.geo / opts.device — optional overrides; server defaults are
         *   USA + UA-inferred when these are blank.
         */
        requestAd: function(opts) {
            var placementId = opts.placementId;
            var elementId = opts.elementId;

            var el = document.getElementById(elementId);
            if (!el) {
                console.error('[adtech] element not found:', elementId);
                return;
            }

            var params = new URLSearchParams({
                placement_id: placementId,
                user_id: state.platformId || ''
            });
            if (opts.geo) params.set('geo', opts.geo);
            if (opts.device) params.set('device', opts.device);

            var url = config.pubadUrl + '/v1/pubad/serve?' + params.toString();

            fetch(url, { credentials: 'omit' })
                .then(function(resp) {
                    if (!resp.ok) throw new Error('pubad ' + resp.status);
                    return resp.json();
                })
                .then(function(data) {
                    if (!data.html) {
                        el.innerHTML = '<div style="text-align:center;color:#999;padding:20px;">No ad available</div>';
                        if (config.debug) console.log('[adtech] no fill', data);
                        return;
                    }
                    renderAd(el, data);
                })
                .catch(function(err) {
                    if (config.debug) console.error('[adtech] request failed:', err);
                    el.innerHTML = '<div style="text-align:center;color:#999;padding:20px;">Ad unavailable</div>';
                });
        },

        /**
         * Opt out at the specified level.
         */
        optOut: function(level) {
            if (level >= 2) {
                deletePlatformId();
            }
            if (config.debug) {
                console.log('[adtech] opt-out level', level);
            }
        },

        /**
         * Get current state (for debugging).
         */
        getState: function() {
            return {
                platformId: state.platformId,
                consent: state.consent,
                pageContext: state.pageContext,
                config: config
            };
        }
    };

    // ============================================================
    // Ad Rendering
    // ============================================================

    function renderAd(el, data) {
        var w = data.width || 0;
        var h = data.height || 0;

        if (w > 0) el.style.width = w + 'px';
        if (h > 0) el.style.minHeight = h + 'px';

        // Server has already substituted macros + signed every URL.
        // Inserting the html as-is lets the creative anchor (the
        // tracker click URL with redir= baked in) work without any
        // client-side URL surgery.
        var imp = data.impression_url || '';
        el.innerHTML = data.html +
            (imp ? '<img src="' + imp + '" width="1" height="1" style="position:absolute;opacity:0;" alt="" />' : '');

        if (data.viewability_url) {
            observeViewability(el, data.viewability_url);
        }
    }

    // ============================================================
    // Viewability Observer
    // ============================================================

    function observeViewability(el, viewabilityURL) {
        if (!('IntersectionObserver' in window)) return;

        var startTime = null;
        var reported = false;

        var observer = new IntersectionObserver(function(entries) {
            var entry = entries[0];
            if (entry.isIntersecting && entry.intersectionRatio >= 0.5) {
                if (!startTime) startTime = Date.now();
            } else if (startTime) {
                startTime = null;
            }

            // IAB: 50% of pixels for ≥1 continuous second for display.
            if (!reported && startTime && (Date.now() - startTime) >= 1000) {
                reported = true;
                var dur = Date.now() - startTime;
                var pct = Math.round(entry.intersectionRatio * 100);
                var sep = viewabilityURL.indexOf('?') === -1 ? '?' : '&';
                fetch(viewabilityURL + sep + 'dur=' + dur + '&pct=' + pct, { credentials: 'omit' });

                if (config.debug) {
                    console.log('[adtech] viewable', { dur: dur, pct: pct });
                }
            }
        }, { threshold: [0, 0.5, 1.0] });

        observer.observe(el);
    }

    // ============================================================
    // Utilities
    // ============================================================

    function getCookie(name) {
        var match = document.cookie.match(new RegExp('(^| )' + name + '=([^;]+)'));
        return match ? match[2] : null;
    }

    function setCookie(name, value, days) {
        var d = new Date();
        d.setTime(d.getTime() + days * 86400000);
        document.cookie = name + '=' + value + ';path=/;expires=' + d.toUTCString() + ';SameSite=Lax';
    }

    function randomHex(len) {
        var arr = new Uint8Array(len);
        crypto.getRandomValues(arr);
        return Array.from(arr, function(b) { return b.toString(16).padStart(2, '0'); }).join('');
    }

    // Export
    window.adtech = adtech;

})(window);
