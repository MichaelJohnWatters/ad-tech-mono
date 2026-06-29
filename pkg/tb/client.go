// Package tb bridges the platform's domain model to TigerBeetle primitives.
//
// Everything here is plumbing: ID conversion, money conversion, code enums,
// and a thin client constructor that hides the SDK's cluster-ID quirk. The
// actual Ledger implementation that uses these pieces lives in
// pkg/billing/tigerbeetle (kept separate so importing the existing memory
// ledger does not pull in the cgo TB client).
package tb

import (
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
	"github.com/tigerbeetle/tigerbeetle-go/pkg/types"
)

// ClusterID is the TB cluster identifier. We run a single cluster everywhere
// (one in local, one in prod) so the ID is a build-time constant.
const ClusterID uint64 = 0

// USDLedger is the TigerBeetle `ledger` field for every USD account and
// transfer. Other currencies would get distinct ledger numbers — out of
// scope for this round (see PLAN.md → "Active Build: TigerBeetle-backed
// Ledger" → "Explicitly out of scope").
const USDLedger uint32 = 1

// ReservationTimeoutSeconds is the TB `timeout` we set on every pending
// reservation transfer. After this elapses TB auto-voids the pending,
// which obsoletes the reservation-expiry cron we would otherwise need.
const ReservationTimeoutSeconds uint32 = 24 * 60 * 60

// Client is the TigerBeetle client surface we depend on. Aliased to the
// SDK's own interface so tests in pkg/billing/tigerbeetle can swap in a
// fake without dragging in the cgo library transitively.
type Client = tigerbeetle.Client

// NewClient connects to a TigerBeetle cluster at addresses (host:port).
// Returns the SDK Client interface so callers can mock for tests.
func NewClient(addresses []string) (Client, error) {
	return tigerbeetle.NewClient(types.ToUint128(ClusterID), addresses)
}
