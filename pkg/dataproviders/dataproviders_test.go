package dataproviders

import (
	"context"
	"testing"
)

func TestDefaultPartyForKind(t *testing.T) {
	cases := map[string]string{
		KindCRM:    PartyFirst,
		KindDMP:    PartyThird,
		KindCDP:    PartyThird,
		KindAgency: PartySecond,
		KindOther:  PartyFirst,
		"unknown":  PartyFirst,
	}
	for kind, want := range cases {
		if got := DefaultPartyForKind(kind); got != want {
			t.Errorf("DefaultPartyForKind(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	p := Provider{Name: "  Acme DMP ", Kind: "DMP"}
	Normalize(&p)
	if p.Name != "Acme DMP" {
		t.Errorf("name not trimmed: %q", p.Name)
	}
	if p.Kind != KindDMP {
		t.Errorf("kind not lowercased: %q", p.Kind)
	}
	if p.DefaultParty != PartyThird {
		t.Errorf("dmp should default to third party, got %q", p.DefaultParty)
	}
	if p.DefaultLicence != LicencePurchased {
		t.Errorf("third party should default to purchased, got %q", p.DefaultLicence)
	}
	if p.DefaultIDType != "user_id" {
		t.Errorf("id_type default = %q", p.DefaultIDType)
	}
	if p.Status != "active" {
		t.Errorf("status default = %q", p.Status)
	}
	if p.NotifyEmails == nil {
		t.Error("notify_emails should be non-nil after normalize")
	}
}

func TestNormalizeCRMIsFirstParty(t *testing.T) {
	p := Provider{Name: "Our CRM", Kind: KindCRM}
	Normalize(&p)
	if p.DefaultParty != PartyFirst || p.DefaultLicence != LicenceFirstParty {
		t.Errorf("crm should be first_party/first, got %q/%q", p.DefaultParty, p.DefaultLicence)
	}
}

func TestValidate(t *testing.T) {
	good := Provider{Name: "x", Kind: KindCRM}
	Normalize(&good)
	if err := Validate(good); err != nil {
		t.Fatalf("valid provider rejected: %v", err)
	}
	if err := Validate(Provider{Name: "", Kind: KindCRM, DefaultParty: PartyFirst, DefaultLicence: LicenceFirstParty, Status: "active"}); err == nil {
		t.Error("empty name should be rejected")
	}
	bad := good
	bad.Kind = "bogus"
	if err := Validate(bad); err == nil {
		t.Error("invalid kind should be rejected")
	}
	bad = good
	bad.DefaultParty = "fourth"
	if err := Validate(bad); err == nil {
		t.Error("invalid party should be rejected")
	}
}

func TestMemoryStoreCRUDAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	p := Provider{AccountID: "acct-a", Name: "Acme", Kind: KindDMP}
	Normalize(&p)
	id, err := s.Create(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Re-saving the same name upserts (no duplicate).
	p.DefaultIDType = "hashed_email"
	if _, err := s.Create(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	list, _ := s.ListByAccount(ctx, "acct-a")
	if len(list) != 1 {
		t.Fatalf("expected 1 provider after upsert, got %d", len(list))
	}
	if list[0].DefaultIDType != "hashed_email" {
		t.Errorf("upsert didn't update id_type: %q", list[0].DefaultIDType)
	}

	// Another tenant can't see or fetch it.
	if other, _ := s.ListByAccount(ctx, "acct-b"); len(other) != 0 {
		t.Errorf("tenant isolation broken on list: %d", len(other))
	}
	if got, _ := s.GetByAccount(ctx, "acct-b", id); got != nil {
		t.Error("tenant isolation broken on get")
	}

	// Owner can fetch + delete.
	if got, _ := s.GetByAccount(ctx, "acct-a", id); got == nil {
		t.Fatal("owner cannot fetch own provider")
	}
	if err := s.DeleteByAccount(ctx, "acct-b", id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ := s.GetByAccount(ctx, "acct-a", id); got == nil {
		t.Error("cross-tenant delete removed the owner's provider")
	}
	if err := s.DeleteByAccount(ctx, "acct-a", id); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if got, _ := s.GetByAccount(ctx, "acct-a", id); got != nil {
		t.Error("provider still present after owner delete")
	}
}
