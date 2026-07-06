package openrtb

// Extended identifiers (OpenRTB 2.6 §3.2.27 EID / §3.2.28 UID) — the standard
// carrier for cookieless identity like Unified ID 2.0 (UID2). A bid request's
// User.EIDs lists one EID per identity provider; each holds one or more UIDs.

// UID2Source is the canonical EID source string for Unified ID 2.0.
const UID2Source = "uidapi.com"

// Agent types for UID.AType (OpenRTB 2.6 §5.28). UID2 is a person-based,
// persistent identifier → atype 3.
const (
	AgentTypeBrowserCookie   = 1 // cookie/device id, resets on clear
	AgentTypePersonInSession = 2 // person-based, tied to a session
	AgentTypePersonPersist   = 3 // person-based, persistent (UID2, EUID, …)
)

// EID is one extended-identifier record from a single source.
type EID struct {
	Source string `json:"source"`
	UIDs   []UID  `json:"uids"`
}

// UID is a single identifier value plus how it was derived.
type UID struct {
	ID    string `json:"id"`
	AType int    `json:"atype,omitempty"`
}

// UID2EID builds the canonical UID2 extended-identifier record for a token.
func UID2EID(token string) EID {
	return EID{Source: UID2Source, UIDs: []UID{{ID: token, AType: AgentTypePersonPersist}}}
}

// UID2From returns the UID2 token carried in a user's EIDs, or "" when absent.
func UID2From(u *User) string {
	if u == nil {
		return ""
	}
	for _, e := range u.EIDs {
		if e.Source != UID2Source {
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

// UserKey returns the most stable identifier for a user: the platform User.ID
// when present, otherwise the UID2 token from EIDs. This lets cookieless users
// (no User.ID but a UID2) still be addressable for opt-out, frequency capping,
// and segment lookups. Returns "" when the user is fully anonymous.
func UserKey(u *User) string {
	if u == nil {
		return ""
	}
	if u.ID != "" {
		return u.ID
	}
	return UID2From(u)
}
