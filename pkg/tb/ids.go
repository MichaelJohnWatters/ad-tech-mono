package tb

import (
	"github.com/google/uuid"
	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
)

// HouseAccountKey is the idgen key for the platform's house account that
// receives margin transfers. Derived once at init so the resulting TB
// account ID is stable across pods and restarts.
const HouseAccountKey = "platform:house"

// EscrowAccountKey is the shared escrow account that holds pending
// reservations during the reserve→settle window.
const EscrowAccountKey = "platform:escrow"

// HouseAccountID is the deterministic TB account ID for the platform house.
var HouseAccountID = mustDeriveAccountID(HouseAccountKey)

// EscrowAccountID is the deterministic TB account ID for shared escrow.
var EscrowAccountID = mustDeriveAccountID(EscrowAccountKey)

// AccountIDFromUUID turns a UUID string (advertiser/publisher row PK) into
// a TB account ID using the raw UUID bytes — no second derivation. Callers
// that already hold a UUID get a 1:1 mapping; the TB account ID and the
// Postgres row ID are byte-equal.
func AccountIDFromUUID(uuidStr string) (types.Uint128, error) {
	u, err := uuid.Parse(uuidStr)
	if err != nil {
		return types.Uint128{}, err
	}
	return types.BytesToUint128([16]byte(u)), nil
}

// AdvertiserAccountID is a convenience for AccountIDFromUUID with an
// advertiser-shaped error path: any parse failure here is a programming
// bug (callers come from the DB or warm cache, both UUID-typed).
func AdvertiserAccountID(advertiserUUID string) (types.Uint128, error) {
	return AccountIDFromUUID(advertiserUUID)
}

// PublisherAccountID mirrors AdvertiserAccountID for publisher accounts.
func PublisherAccountID(publisherUUID string) (types.Uint128, error) {
	return AccountIDFromUUID(publisherUUID)
}

// ReservationID derives a TB transfer ID for a per-trace pending reservation.
// Using idgen.Derive means re-deriving the ID at settle/release time hits
// the same value without storing it in Postgres.
func ReservationID(traceID string) types.Uint128 {
	return mustDeriveTransferID("tb_reservation", traceID)
}

// TraceUserData maps a trace_id string into a Uint128 suitable for the
// transfer.user_data_128 field. Trace IDs in this platform are the OTel
// W3C value (32 hex chars), but synthetic harness IDs use other shapes —
// we derive through idgen.Derive to normalize without imposing a format
// at TB boundaries. Deterministic, so a later QueryTransfers by
// user_data_128 will land on the same value.
func TraceUserData(traceID string) types.Uint128 {
	return mustDeriveTransferID("tb_trace_userdata", traceID)
}

// SettlementID derives a TB transfer ID for the publisher slice of a settle.
// One per trace; deterministic so re-runs are no-ops at TB level.
func SettlementID(traceID string) types.Uint128 {
	return mustDeriveTransferID("tb_settlement", traceID)
}

// MarginTransferID derives a TB transfer ID for the platform margin slice.
func MarginTransferID(traceID string) types.Uint128 {
	return mustDeriveTransferID("tb_margin", traceID)
}

// SpendTransferID derives a TB transfer ID for the publisher slice of a
// direct (CPM) spend. Distinct kind from settlement so a campaign that
// somehow has both events for the same trace doesn't collide.
func SpendTransferID(traceID string) types.Uint128 {
	return mustDeriveTransferID("tb_spend", traceID)
}

// SpendMarginTransferID derives the margin slice ID for a CPM spend.
func SpendMarginTransferID(traceID string) types.Uint128 {
	return mustDeriveTransferID("tb_spend_margin", traceID)
}

// ReleaseTransferID derives the void-pending transfer ID for an explicit
// reservation release. Distinct kind from ReservationID so the void
// transfer has its own unique TB ID.
func ReleaseTransferID(traceID string) types.Uint128 {
	return mustDeriveTransferID("tb_release", traceID)
}

// StatsSourceAccountID is the funding side of every stats bucket transfer
// on StatsLedger. Its balance means nothing (it only ever accumulates
// debits); it exists because TB transfers need two accounts.
var StatsSourceAccountID = mustDeriveAccountID("platform:stats:source")

// StatsBucketAccountID derives the deterministic account ID for one
// summary bucket (e.g. "spend", "reserved", "settled"). The bucket's
// credits_posted IS the lifetime total for that summary field — durable,
// cluster-global, identical from every reporting pod.
func StatsBucketAccountID(bucket string) types.Uint128 {
	return mustDeriveAccountID("platform:stats:" + bucket)
}

// StatTransferID derives the transfer ID for one bucket increment of one
// ledger entry. entryKey is "<entry-type>:<trace-id>", so a redelivered
// event re-derives the same ID and TB's Exists result makes the replay a
// no-op — stats stay exactly-once alongside the money transfers.
func StatTransferID(bucket, entryKey string) types.Uint128 {
	return mustDeriveTransferID("tb_stat_"+bucket, entryKey)
}

// mustDeriveAccountID converts an idgen key into a TB account ID via UUIDv5.
// Used for static accounts (house, escrow) — any error here is a
// build-time bug, panic is fine.
func mustDeriveAccountID(key string) types.Uint128 {
	u, err := uuid.Parse(idgen.Derive("tb_account", key))
	if err != nil {
		panic("tb: failed to derive account ID for " + key + ": " + err.Error())
	}
	return types.BytesToUint128([16]byte(u))
}

func mustDeriveTransferID(kind, key string) types.Uint128 {
	u, err := uuid.Parse(idgen.Derive(kind, key))
	if err != nil {
		panic("tb: failed to derive transfer ID for " + kind + ":" + key + ": " + err.Error())
	}
	return types.BytesToUint128([16]byte(u))
}
