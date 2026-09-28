/*
 * adtechmonoBidAdapter.js — Prebid.js adapter for the adtechmono exchange.
 *
 * This is a standard Prebid.js bidder adapter following the spec in
 * https://docs.prebid.org/dev-docs/bidder-adaptor.html. To use it in
 * production it must be compiled into a Prebid.js build:
 *
 *   1. Drop this file into a Prebid.js checkout at
 *      modules/adtechmonoBidAdapter.js
 *   2. gulp build --modules=adtechmono,<other modules>
 *   3. Serve the resulting prebid.js from your CDN.
 *   4. Publisher pages configure us as a bidder:
 *        pbjs.addAdUnits([{
 *          code: 'div-ad-1',
 *          mediaTypes: { banner: { sizes: [[300, 250]] } },
 *          bids: [{
 *            bidder: 'adtechmono',
 *            params: { placementId: 'pl-news-mpu' }
 *          }]
 *        }]);
 *
 * For the local pub-sim demo we don't run a real Prebid.js build — the
 * "Prebid mode" toggle in web/templates/simulator/minimal.html constructs
 * the same OpenRTB request this adapter would build and POSTs directly to
 * the exchange's /v1/prebid/openrtb2/auction. Keeping the two paths
 * symmetric means any change here should be mirrored in the sim's inline
 * builder (and vice versa).
 *
 * Bidder code: "adtechmono" — locked, see docs/PLAN.md → "Prebid Server
 * Integration". Don't rename without coordinating with any publisher
 * who's already configured us.
 */

import { registerBidder } from '../src/adapters/bidderFactory.js';
import { BANNER } from '../src/mediaTypes.js';

const BIDDER_CODE = 'adtechmono';

// Default endpoints — overridable per-publisher via params.endpoint /
// params.syncUrl so a publisher can point at staging vs prod without an
// adapter rebuild.
const DEFAULT_ENDPOINT = 'https://exchange.adtechmono.example/v1/prebid/openrtb2/auction';
const DEFAULT_SYNC_URL = 'https://exchange.adtechmono.example/v1/prebid/setuid?bidder=adtechmono&f=i';

export const spec = {
  code: BIDDER_CODE,
  supportedMediaTypes: [BANNER],

  // Validate per-ad-unit config before sending. Missing placementId would
  // be useless — the exchange's auction matcher needs it to map to a
  // publisher placement in our DB.
  isBidRequestValid: function (bid) {
    return !!(bid && bid.params && bid.params.placementId);
  },

  // Translate Prebid.js's per-ad-unit bid requests into a single OpenRTB
  // BidRequest. One request batches all the page's eligible ad units
  // (multi-imp). Pub-side floors + sizes flow through verbatim.
  buildRequests: function (validBidRequests, bidderRequest) {
    if (!validBidRequests || validBidRequests.length === 0) {
      return [];
    }

    const imps = validBidRequests.map(function (req) {
      const sizes = (req.sizes && req.sizes.length > 0) ? req.sizes : [[300, 250]];
      return {
        id: req.bidId,
        tagid: req.params.placementId,
        bidfloor: req.params.bidFloor || 0,
        bidfloorcur: req.params.currency || 'USD',
        banner: {
          w: sizes[0][0],
          h: sizes[0][1],
          format: sizes.map(function (s) { return { w: s[0], h: s[1] }; }),
        },
      };
    });

    const referer = (bidderRequest && bidderRequest.refererInfo) || {};

    const openrtbRequest = {
      id: bidderRequest ? bidderRequest.bidderRequestId : ('req-' + Date.now()),
      imp: imps,
      site: {
        domain: referer.domain || (typeof window !== 'undefined' ? window.location.hostname : ''),
        page: referer.page || (typeof window !== 'undefined' ? window.location.href : ''),
      },
      tmax: (bidderRequest && bidderRequest.timeout) || 1000,
    };

    // Forward the publisher's SupplyChain (schain) at source.ext.schain so the
    // exchange can verify the supply path. Prebid's schain module populates it on
    // the bid; passing it through is what lets strict schain enforcement accept us.
    const schain = validBidRequests[0].schain;
    if (schain) {
      openrtbRequest.source = { ext: { schain: schain } };
    }

    // Endpoint override: first valid bid's params win. Publishers using
    // multiple environments per page is rare enough that we don't try to
    // batch per-endpoint.
    const endpoint = validBidRequests[0].params.endpoint || DEFAULT_ENDPOINT;

    return {
      method: 'POST',
      url: endpoint,
      data: JSON.stringify(openrtbRequest),
      options: {
        contentType: 'application/json',
        withCredentials: true, // send our _adtm_uid cookie for identity
      },
    };
  },

  // Parse our OpenRTB BidResponse into the per-imp shape Prebid.js expects.
  // We flatten across seats since the exchange returns one bid per imp
  // and Prebid.js doesn't care about seat grouping at this layer.
  interpretResponse: function (serverResponse, request) {
    const body = serverResponse && serverResponse.body;
    if (!body || body.nobid || !body.seatbid || body.seatbid.length === 0) {
      return [];
    }

    const bids = [];
    body.seatbid.forEach(function (sb) {
      if (!sb.bid) return;
      sb.bid.forEach(function (b) {
        bids.push({
          requestId: b.impid,
          cpm: b.price,
          width: b.w || 300,
          height: b.h || 250,
          ad: b.adm || '',
          adUrl: b.nurl || undefined,
          creativeId: b.crid,
          dealId: b.dealid || undefined,
          currency: body.cur || 'USD',
          netRevenue: true,
          ttl: 300,
          meta: {
            advertiserDomains: b.adomain || [],
          },
        });
      });
    });
    return bids;
  },

  // Cookie sync. Prebid.js will iframe this URL on the page once per
  // user per syncFrequency. The endpoint sets our _adtm_uid cookie which
  // subsequent requestBids calls carry via withCredentials.
  getUserSyncs: function (syncOptions, serverResponses, gdprConsent) {
    if (!syncOptions.iframeEnabled) {
      return [];
    }
    const params = [];
    if (gdprConsent && typeof gdprConsent.gdprApplies !== 'undefined') {
      params.push('gdpr=' + (gdprConsent.gdprApplies ? 1 : 0));
      if (gdprConsent.consentString) {
        params.push('gdpr_consent=' + encodeURIComponent(gdprConsent.consentString));
      }
    }
    const url = DEFAULT_SYNC_URL + (params.length > 0 ? '&' + params.join('&') : '');
    return [{ type: 'iframe', url: url }];
  },
};

registerBidder(spec);
