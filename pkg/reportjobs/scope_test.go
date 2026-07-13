package reportjobs

import (
	"context"
	"testing"
)

type fakeScopeLookup struct {
	types      map[string]string
	publishers map[string][]string
}

func (f fakeScopeLookup) AccountType(_ context.Context, id string) (string, error) {
	return f.types[id], nil
}
func (f fakeScopeLookup) PublisherIDs(_ context.Context, id string) ([]string, error) {
	return f.publishers[id], nil
}

func TestResolveTenantFilters(t *testing.T) {
	lookup := fakeScopeLookup{
		types: map[string]string{
			"adv":    "advertiser",
			"agency": "agency",
			"pub1":   "publisher",
			"pubN":   "publisher",
			"staff":  "staff",
			"weird":  "martian",
		},
		publishers: map[string][]string{
			"pub1": {"p-100"},
			"pubN": {"p-200", "p-201"},
		},
	}
	ctx := context.Background()

	t.Run("advertiser_forces_account_id", func(t *testing.T) {
		got, err := ResolveTenantFilters(ctx, lookup, "adv", map[string]string{"account_id": "someone-else"})
		if err != nil {
			t.Fatal(err)
		}
		if got["account_id"] != "adv" {
			t.Errorf("account_id = %s, want adv (forced)", got["account_id"])
		}
	})

	t.Run("agency_forces_account_id", func(t *testing.T) {
		got, err := ResolveTenantFilters(ctx, lookup, "agency", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got["account_id"] != "agency" {
			t.Errorf("account_id = %s", got["account_id"])
		}
	})

	t.Run("single_publisher_injected", func(t *testing.T) {
		got, err := ResolveTenantFilters(ctx, lookup, "pub1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if got["publisher_id"] != "p-100" {
			t.Errorf("publisher_id = %s, want p-100", got["publisher_id"])
		}
	})

	t.Run("owned_publisher_kept", func(t *testing.T) {
		got, err := ResolveTenantFilters(ctx, lookup, "pubN", map[string]string{"publisher_id": "p-201"})
		if err != nil {
			t.Fatal(err)
		}
		if got["publisher_id"] != "p-201" {
			t.Errorf("publisher_id = %s, want p-201", got["publisher_id"])
		}
	})

	t.Run("foreign_publisher_rejected", func(t *testing.T) {
		if _, err := ResolveTenantFilters(ctx, lookup, "pub1", map[string]string{"publisher_id": "p-999"}); err == nil {
			t.Error("expected error for publisher the account does not own")
		}
	})

	t.Run("multiple_publishers_require_filter", func(t *testing.T) {
		if _, err := ResolveTenantFilters(ctx, lookup, "pubN", nil); err == nil {
			t.Error("expected error when multi-publisher account names no publisher_id")
		}
	})

	t.Run("staff_untouched", func(t *testing.T) {
		got, err := ResolveTenantFilters(ctx, lookup, "staff", map[string]string{"campaign_id": "c-1"})
		if err != nil {
			t.Fatal(err)
		}
		if got["account_id"] != "" || got["campaign_id"] != "c-1" {
			t.Errorf("staff filters = %v, want untouched", got)
		}
	})

	t.Run("unknown_type_rejected", func(t *testing.T) {
		if _, err := ResolveTenantFilters(ctx, lookup, "weird", nil); err == nil {
			t.Error("expected error for unknown account type")
		}
	})

	t.Run("input_not_mutated", func(t *testing.T) {
		in := map[string]string{"account_id": "original"}
		if _, err := ResolveTenantFilters(ctx, lookup, "adv", in); err != nil {
			t.Fatal(err)
		}
		if in["account_id"] != "original" {
			t.Errorf("input map mutated: %v", in)
		}
	})
}
