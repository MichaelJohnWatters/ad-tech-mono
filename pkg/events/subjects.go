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

	// Auction subjects (Exchange → DSP, Reporting)
	SubjectAuctionWin      = "adtech.auction.win"
	SubjectAuctionComplete = "adtech.auction.complete"

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
	SubjectCacheInvalidateOptOuts      = "adtech.cache.invalidate.opt-outs"
	SubjectCacheInvalidateWebhookSubs  = "adtech.cache.invalidate.webhook-subs"
	SubjectCacheInvalidateBillingRates = "adtech.cache.invalidate.billing-rates"
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
