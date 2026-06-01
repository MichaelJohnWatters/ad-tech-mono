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

	// Auction subjects (Exchange → DSP, Reporting)
	SubjectAuctionWin      = "adtech.auction.win"
	SubjectAuctionComplete = "adtech.auction.complete"

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

	// Webhooks (Any → Webhooks dispatcher)
	SubjectWebhook = "adtech.webhooks"

	// Stream name
	StreamName = "adtech"
	// Stream subject wildcard
	StreamSubjects = "adtech.>"
)
