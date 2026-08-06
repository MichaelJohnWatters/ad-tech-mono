package tb

import (
	"testing"

	"github.com/google/uuid"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
)

func TestAccountIDFromUUIDRoundTrip(t *testing.T) {
	in := idgen.Derive("account", "adv-acme")
	id, err := AccountIDFromUUID(in)
	if err != nil {
		t.Fatalf("AccountIDFromUUID(%q): %v", in, err)
	}
	b := id.Bytes()
	got, err := uuid.FromBytes(b[:])
	if err != nil {
		t.Fatalf("uuid.FromBytes: %v", err)
	}
	if got.String() != in {
		t.Fatalf("round-trip mismatch: in=%q out=%q", in, got)
	}
}

func TestAccountIDFromUUIDRejectsInvalid(t *testing.T) {
	if _, err := AccountIDFromUUID("not-a-uuid"); err == nil {
		t.Fatal("expected error for invalid UUID, got nil")
	}
}

func TestStaticAccountIDsStable(t *testing.T) {
	// Re-derive and compare — guards against accidental changes to the
	// derivation scheme that would invalidate every TB house balance.
	wantHouse := mustDeriveAccountID(HouseAccountKey)
	if HouseAccountID != wantHouse {
		t.Fatalf("HouseAccountID drifted: have %v want %v", HouseAccountID, wantHouse)
	}
	wantEscrow := mustDeriveAccountID(EscrowAccountKey)
	if EscrowAccountID != wantEscrow {
		t.Fatalf("EscrowAccountID drifted: have %v want %v", EscrowAccountID, wantEscrow)
	}
}

func TestStaticAccountsDistinct(t *testing.T) {
	if HouseAccountID == EscrowAccountID {
		t.Fatal("house and escrow account IDs must differ")
	}
}

func TestReservationIDDeterministic(t *testing.T) {
	a := ReservationID("trace-abc")
	b := ReservationID("trace-abc")
	if a != b {
		t.Fatalf("ReservationID not deterministic: %v vs %v", a, b)
	}
	c := ReservationID("trace-xyz")
	if a == c {
		t.Fatal("different trace IDs must produce different reservation IDs")
	}
}

func TestTransferIDFamiliesDistinct(t *testing.T) {
	trace := "trace-abc"
	ids := map[string]any{
		"reservation":  ReservationID(trace),
		"settlement":   SettlementID(trace),
		"margin":       MarginTransferID(trace),
		"spend":        SpendTransferID(trace),
		"spend_margin": SpendMarginTransferID(trace),
	}
	seen := make(map[any]string, len(ids))
	for kind, id := range ids {
		if other, dup := seen[id]; dup {
			t.Fatalf("transfer ID collision for trace=%q: %s and %s both produce %v", trace, kind, other, id)
		}
		seen[id] = kind
	}
}
