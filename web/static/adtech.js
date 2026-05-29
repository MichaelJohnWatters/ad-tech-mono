/**
 * Ad Tech Platform - Publisher Ad Tag SDK
 *
 * Drop-in script for publishers to serve ads on their sites.
 * Handles: platform ID management, bid requests via SSP, ad rendering,
 * impression/click/viewability tracking, consent, contextual signals.
 *
 * Usage:
 *   <script src="https://cdn.adtech.example/adtech.js"></script>
 *   <script>
 *     adtech.init({ publisherId: 'pub-123', siteId: 'site-456' });
 *     adtech.setConsent({ gdpr: true, purposes: [1,2,3,4] });
 *     adtech.setPageContext({ categories: ['IAB17'], keywords: ['football'] });
 *     adtech.requestAd({ placementId: 'pl-001', size: '300x250', elementId: 'ad-slot-1' });
 *   </script>
 */
(function(window) {
    'use strict';

    var SDK_VERSION = '1.0.0';
    var COOKIE_NAME = 'adtech_uid';
    var COOKIE_DAYS = 90;

    var config = {
        publisherId: null,
        siteId: null,
        sspUrl: null,      // auto-detected or overridden
        trackerUrl: null,
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
         */
        init: function(opts) {
            config.publisherId = opts.publisherId || null;
            config.siteId = opts.siteId || null;
            config.sspUrl = opts.sspUrl || (window.location.protocol + '//' + window.location.hostname + ':8084');
            config.trackerUrl = opts.trackerUrl || (window.location.protocol + '//' + window.location.hostname + ':8083');
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
         */
        requestAd: function(opts) {
            var placementId = opts.placementId;
            var size = opts.size || '300x250';
            var elementId = opts.elementId;
            var parts = size.split('x');
            var w = parseInt(parts[0]);
            var h = parseInt(parts[1]);

            var el = document.getElementById(elementId);
            if (!el) {
                console.error('[adtech] element not found:', elementId);
                return;
            }

            // Build SSP request
            var params = new URLSearchParams({
                placement_id: placementId,
                geo: '', // server-side geo detection
                device: isMobile() ? 'mobile' : 'desktop',
                user_id: state.platformId || ''
            });

            var url = config.sspUrl + '/v1/ssp/request?' + params.toString();

            fetch(url)
                .then(function(resp) { return resp.json(); })
                .then(function(data) {
                    if (!data.bid_response || data.bid_response.nobid) {
                        el.innerHTML = '<div style="text-align:center;color:#999;padding:20px;">No ad available</div>';
                        return;
                    }

                    var winner = data.bid_response.seatbid[0].bid[0];
                    renderAd(el, winner, data, w, h);
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

    function renderAd(el, winner, data, w, h) {
        var traceId = data.trace_id;
        var trackerUrl = config.trackerUrl;

        // Build impression pixel URL
        var impParams = new URLSearchParams({
            tid: traceId,
            cid: winner.cid || '',
            crid: winner.crid || '',
            pid: data.placement_id || '',
            pubid: data.publisher_id || '',
            price: winner.price || '0',
            cur: data.bid_response.cur || 'USD',
            w: w, h: h
        });
        var impUrl = trackerUrl + '/v1/t/imp?' + impParams.toString();

        // Render ad
        el.style.width = w + 'px';
        el.style.minHeight = h + 'px';
        el.innerHTML =
            '<div style="width:' + w + 'px;height:' + h + 'px;background:#f8f8f8;border:1px solid #ddd;' +
            'display:flex;flex-direction:column;align-items:center;justify-content:center;font-family:sans-serif;">' +
            '<p style="color:#666;font-size:13px;">Campaign: ' + (winner.cid || '') + '</p>' +
            '<p style="color:#666;font-size:12px;">Creative: ' + (winner.crid || '') + '</p>' +
            '</div>' +
            '<img src="' + impUrl + '" width="1" height="1" style="position:absolute;opacity:0;">';

        // Viewability tracking
        observeViewability(el, traceId, winner, data);
    }

    // ============================================================
    // Viewability Observer
    // ============================================================

    function observeViewability(el, traceId, winner, data) {
        if (!('IntersectionObserver' in window)) return;

        var startTime = null;
        var totalVisible = 0;
        var reported = false;

        var observer = new IntersectionObserver(function(entries) {
            var entry = entries[0];
            if (entry.isIntersecting && entry.intersectionRatio >= 0.5) {
                if (!startTime) startTime = Date.now();
            } else {
                if (startTime) {
                    totalVisible += Date.now() - startTime;
                    startTime = null;
                }
            }

            // Report viewability after 1 second of 50%+ visibility
            if (!reported && startTime && (Date.now() - startTime) >= 1000) {
                reported = true;
                var pct = Math.round(entry.intersectionRatio * 100);
                var dur = Date.now() - startTime;

                var viewParams = new URLSearchParams({
                    tid: traceId,
                    cid: winner.cid || '',
                    pid: data.placement_id || '',
                    pubid: data.publisher_id || '',
                    dur: dur, pct: pct
                });
                fetch(config.trackerUrl + '/v1/t/view?' + viewParams.toString());

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

    function isMobile() {
        return /Mobi|Android|iPhone|iPad/i.test(navigator.userAgent);
    }

    // Export
    window.adtech = adtech;

})(window);
