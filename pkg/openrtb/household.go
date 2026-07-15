package openrtb

// Household identity (CTV). There is no first-class household field in
// OpenRTB — the industry carries household IDs as user.eids entries scoped to
// a household-graph source (TransUnion/Tru Optik, Experian, LiveRamp all work
// this way). We do the same with our own source string, so external vendor
// household EIDs and ours coexist in the same list.

// HouseholdSource is the EID source string for the platform's own household
// identifier (v1: derived at the SSP from a salted hash of the client IP —
// the industry's de facto household proxy).
const HouseholdSource = "adtech.household"

// HouseholdAType is the UID.AType for household-scoped identifiers. The base
// AdCOM agent-type enum has no household value; 500+ is the vendor-specific
// range (OpenRTB 2.6 §5.28).
const HouseholdAType = 501

// HouseholdEID builds the extended-identifier record for a household id.
func HouseholdEID(id string) EID {
	return EID{Source: HouseholdSource, UIDs: []UID{{ID: id, AType: HouseholdAType}}}
}

// HouseholdFrom returns the platform household id carried in a user's EIDs,
// or "" when absent.
func HouseholdFrom(u *User) string {
	if u == nil {
		return ""
	}
	for _, e := range u.EIDs {
		if e.Source != HouseholdSource {
			continue
		}
		for _, uid := range e.UIDs {
			if uid.ID != "" {
				return uid.ID
			}
		}
	}
	return ""
}
