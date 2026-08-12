package ara

import (
	"encoding/json"
	"time"
)

// SourceRegistration is a recorded attribution source. We persist it at
// impression time so the (unauthenticated, cross-tenant) report-ingest endpoint
// can later resolve which advertiser account a browser-posted report belongs to.
type SourceRegistration struct {
	SourceEventID string
	AccountID     string
	Destination   string
	CampaignID    string
	ExpiresAt     time.Time
}

// StoredReport is a persisted ARA report (the read model for the reporting-only
// overlay). Body is the raw report as the browser posted it; for aggregatable
// reports its payloads stay encrypted.
type StoredReport struct {
	ID                     string
	ReportType             ReportType
	AttributionDestination string
	SourceEventID          string
	TriggerData            string
	ReportID               string
	Body                   json.RawMessage
	ReceivedAt             time.Time
}
