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
                    if (data.no_fill || data.nobid || !data.html) {
                        var reason = data.reason || 'no_fill';
                        var label = noFillLabel(reason);
                        el.innerHTML = '<div style="text-align:center;color:#999;padding:20px;font-size:13px;">' +
                            label + '<div style="font-size:11px;color:#bbb;margin-top:4px;">reason: ' + reason + '</div></div>';
                        if (config.debug) console.log('[adtech] no fill', { reason: reason, trace_id: data.trace_id });
                        return;
                    }
                    renderAd(el, data);
                })
                .catch(function(err) {
                    if (config.debug) console.error('[adtech] request failed:', err);
                    el.innerHTML = '<div style="text-align:center;color:#999;padding:20px;">Ad unavailable (transport error)</div>';
                });
        },

        /**
         * Request and render a VIDEO ad (VAST). Same options as requestAd
         * (placementId, elementId, geo, device). Renders an inline muted-autoplay
         * player, fires the impression + quartile beacons, and self-measures IAB
         * VIDEO viewability (50% on-screen for 2 continuous seconds) — the same
         * shared observeViewability the display path uses, just a 2s dwell.
         */
        requestVideoAd: function(opts) {
            var el = document.getElementById(opts.elementId);
            if (!el) {
                console.error('[adtech] element not found:', opts.elementId);
                return;
            }
            var params = new URLSearchParams({
                placement_id: opts.placementId,
                user_id: state.platformId || ''
            });
            if (opts.geo) params.set('geo', opts.geo);
            if (opts.device) params.set('device', opts.device);

            fetch(config.pubadUrl + '/v1/pubad/video/vast?' + params.toString(), { credentials: 'omit' })
                .then(function(resp) {
                    if (!resp.ok) throw new Error('pubad video ' + resp.status);
                    return resp.text();
                })
                .then(function(xml) { renderVASTAd(el, xml, false); })
                .catch(function(err) {
                    if (config.debug) console.error('[adtech] video request failed:', err);
                    el.innerHTML = '<div style="text-align:center;color:#999;padding:20px;">Video ad unavailable</div>';
                });
        },

        /**
         * Request and render a NATIVE ad (OpenRTB Native 1.2). The server returns
         * a ready-to-inject HTML card with the impression pixel already embedded
         * (fires on render) and the click wrapped in the signed tracker anchor —
         * so the client just injects it. 204 = honest no-fill.
         */
        requestNativeAd: function(opts) {
            var el = document.getElementById(opts.elementId);
            if (!el) {
                console.error('[adtech] element not found:', opts.elementId);
                return;
            }
            var params = new URLSearchParams({
                placement_id: opts.placementId,
                user_id: state.platformId || ''
            });
            if (opts.geo) params.set('geo', opts.geo);
            if (opts.device) params.set('device', opts.device);

            fetch(config.pubadUrl + '/v1/pubad/native?' + params.toString(), { credentials: 'omit' })
                .then(function(resp) { return resp.status === 204 ? '' : resp.text(); })
                .then(function(html) {
                    el.innerHTML = (html && html.trim())
                        ? html
                        : '<div style="text-align:center;color:#999;padding:20px;">No ad available</div>';
                })
                .catch(function(err) {
                    if (config.debug) console.error('[adtech] native request failed:', err);
                });
        },

        /**
         * Request and render an AUDIO ad (VAST). Renders an inline <audio> player
         * and fires the impression + quartile beacons. Audio has no viewability.
         */
        requestAudioAd: function(opts) {
            var el = document.getElementById(opts.elementId);
            if (!el) {
                console.error('[adtech] element not found:', opts.elementId);
                return;
            }
            var params = new URLSearchParams({
                placement_id: opts.placementId,
                user_id: state.platformId || ''
            });
            if (opts.geo) params.set('geo', opts.geo);
            if (opts.device) params.set('device', opts.device);

            fetch(config.pubadUrl + '/v1/pubad/audio?' + params.toString(), { credentials: 'omit' })
                .then(function(resp) {
                    if (!resp.ok) throw new Error('pubad audio ' + resp.status);
                    return resp.text();
                })
                .then(function(xml) { renderVASTAd(el, xml, true); })
                .catch(function(err) {
                    if (config.debug) console.error('[adtech] audio request failed:', err);
                    el.innerHTML = '<div style="text-align:center;color:#999;padding:20px;">Audio ad unavailable</div>';
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
            observeViewability(el, data.viewability_url, 1000); // display: 1s dwell
        }
    }

    // renderVASTAd parses a VAST document and renders an inline linear ad —
    // <video> (isAudio=false) or <audio> (isAudio=true) — wiring the standard
    // VAST beacons: impression + start on play, quartiles on timeupdate, complete
    // on ended. Video additionally self-measures IAB viewability (2s dwell) once
    // it's playing. Every tracker URL in the VAST is already server-signed; the
    // client only fires them (pixel = new Image().src, no CORS needed).
    function renderVASTAd(el, xml, isAudio) {
        var doc = new DOMParser().parseFromString(xml, 'text/xml');
        var media = doc.querySelector('MediaFile');
        if (!media) {
            el.innerHTML = '<div style="text-align:center;color:#999;padding:20px;">No ' + (isAudio ? 'audio' : 'video') + ' ad</div>';
            return;
        }
        var imp = (doc.querySelector('Impression') || {}).textContent;
        var track = {};
        doc.querySelectorAll('Tracking').forEach(function(t) { track[t.getAttribute('event')] = (t.textContent || '').trim(); });

        var m = document.createElement(isAudio ? 'audio' : 'video');
        m.controls = true;
        m.style.width = '100%';
        if (!isAudio) {
            m.setAttribute('playsinline', '');
            m.muted = true; // muted autoplay is allowed without a user gesture
        }
        m.src = media.textContent.trim();
        el.innerHTML = '';
        el.appendChild(m);

        var fired = {};
        function beacon(u) { if (u) { (new Image()).src = u; } }
        m.addEventListener('play', function() {
            if (!fired.imp) {
                fired.imp = 1;
                beacon(imp);
                beacon(track.start);
                // IAB video viewability: 50% on-screen for 2 continuous seconds.
                if (!isAudio && track.viewable) observeViewability(m, track.viewable, 2000);
            }
        });
        m.addEventListener('timeupdate', function() {
            if (!m.duration) return;
            var p = m.currentTime / m.duration;
            if (p >= 0.25 && !fired.q1) { fired.q1 = 1; beacon(track.firstQuartile); }
            if (p >= 0.50 && !fired.q2) { fired.q2 = 1; beacon(track.midpoint); }
            if (p >= 0.75 && !fired.q3) { fired.q3 = 1; beacon(track.thirdQuartile); }
        });
        m.addEventListener('ended', function() { if (!fired.done) { fired.done = 1; beacon(track.complete); } });

        // Video autoplays (muted); audio waits for the user's play control.
        if (!isAudio) { var pp = m.play(); if (pp && pp.catch) pp.catch(function() {}); }
    }

    // ============================================================
    // Viewability Observer
    // ============================================================

    // observeViewability self-measures IAB viewability on an element and fires the
    // signed viewability beacon once it's been >=50% on-screen for dwellMs
    // continuous milliseconds. dwellMs defaults to 1000 (display); callers pass
    // 2000 for video (the IAB video standard). The server recomputes the verdict
    // from the reported dur/pct/area — the client only measures.
    //
    // IntersectionObserver callbacks fire only on intersection CHANGES, so a
    // static (unscrolled) element that lands >=50% visible would never re-trigger
    // the dwell check. We therefore track visibility in the observer and POLL the
    // dwell on a timer — so a video sitting still in view still reports.
    function observeViewability(el, viewabilityURL, dwellMs) {
        if (!('IntersectionObserver' in window)) return;
        dwellMs = dwellMs || 1000;

        var visibleSince = null, lastRatio = 0, reported = false;
        var observer = new IntersectionObserver(function(entries) {
            var entry = entries[entries.length - 1];
            lastRatio = entry.intersectionRatio;
            if (entry.isIntersecting && entry.intersectionRatio >= 0.5) {
                if (!visibleSince) visibleSince = Date.now();
            } else {
                visibleSince = null;
            }
        }, { threshold: [0, 0.25, 0.5, 0.75, 1.0] });
        observer.observe(el);

        var poll = setInterval(function() {
            if (reported) { clearInterval(poll); return; }
            if (visibleSince && (Date.now() - visibleSince) >= dwellMs) {
                reported = true;
                clearInterval(poll);
                observer.disconnect();
                var dur = Date.now() - visibleSince;
                var pct = Math.round(lastRatio * 100);
                var area = (el.offsetWidth * el.offsetHeight) || 0;
                var sep = viewabilityURL.indexOf('?') === -1 ? '?' : '&';
                fetch(viewabilityURL + sep + 'dur=' + dur + '&pct=' + pct + '&area=' + area, { credentials: 'omit' });
                if (config.debug) console.log('[adtech] viewable', { dur: dur, pct: pct, area: area, dwellMs: dwellMs });
            }
        }, 200);
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

    // noFillLabel maps a backend reason token (returned in {reason: ...})
    // to a publisher-friendly message rendered in the slot when there's
    // no ad to show. Unknown reasons fall through to a generic label so
    // a new backend reason doesn't blow up the slot — just shows the raw
    // token so ops can grep without an SDK redeploy.
    function noFillLabel(reason) {
        switch (reason) {
            case 'freqcap':
                return 'Ad cap reached for this session';
            case 'no_eligible_campaigns':
                return 'No matching ads for this slot';
            case 'all_bids_below_floor':
                return 'No bids cleared the price floor';
            case 'ssp_unavailable':
            case 'adserver_unavailable':
            case 'adserver_bad_response':
                return 'Ad service temporarily unavailable';
            case 'programmatic-nobid-and-no-house':
                return 'No demand for this slot';
            case 'arbitration-default-branch':
                return 'No ad available';
            default:
                return 'No ad available';
        }
    }

    // Export
    window.adtech = adtech;

})(window);
