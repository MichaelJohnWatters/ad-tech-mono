package audiencemappings

import (
	"context"
	"testing"
)

func TestValidateMapping(t *testing.T) {
	tests := []struct {
		name    string
		m       Mapping
		wantErr bool
	}{
		{
			name: "valid id_value only",
			m:    Mapping{Mappings: map[string]string{"customer_email": "id_value"}},
		},
		{
			name: "valid id_value + id_type",
			m:    Mapping{Mappings: map[string]string{"cust_id": "id_value", "kind": "id_type"}},
		},
		{
			name:    "non-consumed target rejected",
			m:       Mapping{Mappings: map[string]string{"cust_id": "id_value", "age": "age_bucket"}},
			wantErr: true,
		},
		{
			name:    "no id_value rejected",
			m:       Mapping{Mappings: map[string]string{"kind": "id_type"}},
			wantErr: true,
		},
		{
			name:    "empty mapping rejected",
			m:       Mapping{Mappings: map[string]string{}},
			wantErr: true,
		},
		{
			name:    "empty source column rejected",
			m:       Mapping{Mappings: map[string]string{"": "id_value"}},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMapping(tc.m)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestMemoryStoreTenantIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	idA, err := s.Create(ctx, Mapping{AccountID: "acct-A", Name: "crm", Mappings: map[string]string{"email": "id_value"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, Mapping{AccountID: "acct-B", Name: "crm", Mappings: map[string]string{"uid": "id_value"}}); err != nil {
		t.Fatal(err)
	}

	// A's list excludes B's.
	listA, err := s.ListByAccount(ctx, "acct-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].AccountID != "acct-A" {
		t.Fatalf("account A list leaked or missing: %+v", listA)
	}

	// B can't Get A's mapping.
	got, err := s.GetByAccount(ctx, "acct-B", idA)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("account B read account A's mapping: %+v", got)
	}

	// B can't delete A's mapping.
	if err := s.DeleteByAccount(ctx, "acct-B", idA); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetByAccount(ctx, "acct-A", idA); got == nil {
		t.Fatalf("account B deleted account A's mapping")
	}
}

func TestMemoryStoreNameUpsert(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	id1, err := s.Create(ctx, Mapping{AccountID: "acct-A", Name: "crm", Mappings: map[string]string{"email": "id_value"}, IDType: "hashed_email"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.Create(ctx, Mapping{AccountID: "acct-A", Name: "crm", Mappings: map[string]string{"uid": "id_value"}, IDType: "user_id"})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("re-saving the same name should upsert (same id): %s vs %s", id1, id2)
	}
	list, _ := s.ListByAccount(ctx, "acct-A")
	if len(list) != 1 {
		t.Fatalf("expected 1 mapping after upsert, got %d", len(list))
	}
	if list[0].IDType != "user_id" || list[0].Mappings["uid"] != "id_value" {
		t.Fatalf("upsert did not overwrite fields: %+v", list[0])
	}
}
