package events

// NATS subject constants. All services import these instead of hardcoding strings.
const (
	// Event subjects (Tracker → Reporting)
	SubjectImpression = "adtech.events.impression"
	SubjectClick      = "adtech.events.click"
	SubjectConversion = "adtech.events.conversion"
	SubjectView       = "adtech.events.view"
	SubjectVideo      = "adtech.events.video"
	SubjectAudio      = "adtech.events.audio"
	// SubjectTrackerRejected — fires whenever the tracker drops a pixel
	// before recording it (HMAC strict-mode reject, fraud check
	// blocked, dedup hit). Consumed by reporting for ops dashboards:
	// fraud-volume alerts + "we blocked X% of fraudulent traffic" for
	// advertiser reports + true-cost-per-acquisition that subtracts
	// rejected traffic from the denominator. Distinct from the
	// per-event-type subjects above (those carry successful events).
	SubjectTrackerRejected = "adtech.tracker.rejected"
	// SubjectAdserverRenderFailed — fires when the ad server can't
	// resolve the requested creative (unknown_creative) or hits an
	// internal error and falls back to the default placeholder HTML.
	// The impression pixel still fires (we don't reveal the failure to
	// the browser) so reporting needs this side-channel to surface
	// "creative X is broken" without log scraping. Consumed by
	// reporting for ops dashboards.
	SubjectAdserverRenderFailed = "adtech.adserver.render_failed"
	// SubjectAdserverFreqCapBlocked — fires when the ad server
	// suppresses a serve because the (user, campaign) frequency cap
	// counter is saturated. Distinct from tracker.rejected (those are
	// post-serve drops); this is a pre-serve drop with no impression
	// produced. Lets ops alert on suppression-rate change and gives
	// advertisers visibility into "we suppressed N over-cap serves"
	// without scraping logs.
	SubjectAdserverFreqCapBlocked = "adtech.adserver.freq_cap_blocked"

	// Auction subjects (Exchange → DSP, Reporting)
	SubjectAuctionWin      = "adtech.auction.win"
	SubjectAuctionComplete = "adtech.auction.complete"

	// SubjectDSPCall fires once per DSP fan-out call per auction (Exchange →
	// Reporting). Per-DSP routing telemetry — bid received?, price, latency,
	// timeout — feeding the dsp_calls analytics table and the SmartRouter
	// warm-start. Higher volume than auction.win (one per DSP, not per
	// auction), so it's sample-able at the exchange.
	SubjectDSPCall = "adtech.optimise.dsp_call"

	// Publisher-adserver served-impression subjects. SubjectDirectWin
	// fires every time a direct-sold line item (sponsorship / guaranteed
	// / house) gets served; SubjectPrebidOutboundWin fires when an
	// external Prebid Server's bid wins the programmatic comparison
	// against our SSP. Both consumed by reporting to close the analytics
	// gaps where these paths historically left no record.
	SubjectDirectWin         = "adtech.direct.win"
	SubjectPrebidOutboundWin = "adtech.prebid.outbound.win"
	// ServeNoFill fires when the publisher-adserver exhausted every
	// demand source for a request. Consumed by reporting for fill-rate
	// analytics — without it the only signal of "this request returned
	// nothing" was a log line.
	SubjectServeNoFill = "adtech.serve.nofill"

	// Budget subjects (DSP → Exchange)
	SubjectBudgetDepleted = "adtech.budget.depleted"

	// BalanceDepleted fires when an advertiser's prepay balance hits zero —
	// the account-level sibling of BudgetDepleted. Published by the DSP
	// (bid-path gate) and consumable by webhooks/reporting so the
	// advertiser learns funds ran out.
	SubjectBalanceDepleted = "adtech.balance.depleted"

	// Campaign lifecycle (DSP → Reporting, Webhooks)
	SubjectCampaignStateChanged = "adtech.campaign.state_changed"

	// Privacy (Gateway → ALL services)
	SubjectPrivacyOptOut    = "adtech.privacy.opt_out"
	SubjectPrivacyDeletion  = "adtech.privacy.deletion_requested"
	SubjectPrivacyCompleted = "adtech.privacy.deletion_completed"

	// Cache invalidation (Any → ALL services, Core NATS pub/sub not JetStream)
	SubjectCacheInvalidateCampaigns    = "adtech.cache.invalidate.campaigns"
	SubjectCacheInvalidatePlacements   = "adtech.cache.invalidate.placements"
	SubjectCacheInvalidateCreatives    = "adtech.cache.invalidate.creatives"
	SubjectCacheInvalidateDSPs         = "adtech.cache.invalidate.dsp-endpoints"
	SubjectCacheInvalidatePublishers   = "adtech.cache.invalidate.publishers"
	SubjectCacheInvalidateDeals        = "adtech.cache.invalidate.deals"
	SubjectCacheInvalidateFraudRules   = "adtech.cache.invalidate.fraud-rules"
	SubjectCacheInvalidateAdsTxt       = "adtech.cache.invalidate.ads-txt"
	SubjectCacheInvalidateAudience     = "adtech.cache.invalidate.audience"
	SubjectCacheInvalidateOptOuts      = "adtech.cache.invalidate.opt-outs"
	SubjectCacheInvalidateWebhookSubs  = "adtech.cache.invalidate.webhook-subs"
	SubjectCacheInvalidateBillingRates = "adtech.cache.invalidate.billing-rates"
	// AdvertiserBalances: published by the gateway on topup (credit) and by
	// reporting's billing sink on spend drawdown (throttled per account).
	// Subscribed by the DSP's balance warm cache so the bid-path funds gate
	// rebases within NATS RTT instead of the 30s poll.
	SubjectCacheInvalidateAdvertiserBalances = "adtech.cache.invalidate.advertiser-balances"
	SubjectCacheInvalidateSigningKeys  = "adtech.cache.invalidate.signing-keys"
	// Publisher-side direct-sold line items consumed by cmd/publisher-adserver.
	SubjectCacheInvalidatePublisherLineItems = "adtech.cache.invalidate.publisher-line-items"
	// Live config table — published by pkg/config.Manager.Set whenever a
	// value is written via PUT /v1/config, so every other pod re-polls
	// immediately instead of waiting for its 30s tick.
	SubjectCacheInvalidateConfig = "adtech.cache.invalidate.config"
	// Secrets table — published on every create / rotate / revoke. Every
	// service holding a secrets warm cache re-polls on receipt so a
	// rotation reaches all pods within NATS round-trip time. Sub-second
	// propagation enables the "rotate via UI without redeploy" flow.
	SubjectCacheInvalidateSecrets = "adtech.cache.invalidate.secrets"

	// Webhooks (Any → Webhooks dispatcher)
	SubjectWebhook = "adtech.webhooks"

	// Stream name
	StreamName = "adtech"
	// Stream subject wildcard
	StreamSubjects = "adtech.>"
)
