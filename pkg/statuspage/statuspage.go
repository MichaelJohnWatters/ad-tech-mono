// Package statuspage powers the public status page (PLAN Phase 11, item 107).
// It rolls the platform's internal service health up into a handful of
// customer-facing COMPONENTS (so the page speaks "Bidding & Auctions", not
// "dsp/ssp/exchange"), combines that with staff-authored incidents, and reports
// one overall status. The rollup is a pure function so it is trivially testable;
// the gateway feeds it live /readyz probe results + incidents from Postgres.
package statuspage

import (
	"context"
	"sort"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
)

// Component status levels, worst-last so they order/compare naturally.
const (
	StatusOperational = "operational"       // all backing services ready
	StatusDegraded    = "degraded"          // some backing services ready
	StatusDown        = "major_outage"      // no backing services ready
	StatusMaintenance = "under_maintenance" // reserved (not auto-derived)
)

// statusRank orders levels from best to worst for the overall roll-up.
var statusRank = map[string]int{
	StatusOperational: 0,
	StatusMaintenance: 1,
	StatusDegraded:    2,
	StatusDown:        3,
}

// ComponentDef maps a public component to the internal services that back it.
type ComponentDef struct {
	Name     string
	Services []string
}

// DefaultComponents is the public component catalog. Ordering here is the
// display order on the page. Deliberately coarse — it hides internal topology.
func DefaultComponents() []ComponentDef {
	return []ComponentDef{
		{Name: "Bidding & Auctions", Services: []string{constants.ServiceDSP, constants.ServiceSSP, constants.ServiceExchange}},
		{Name: "Ad Serving", Services: []string{constants.ServiceAdServer, constants.ServicePublisherAdServer, constants.ServiceSSAI}},
		{Name: "Event Tracking", Services: []string{constants.ServiceTracker}},
		{Name: "Reporting & Dashboards", Services: []string{constants.ServiceReporting, constants.ServiceGateway}},
		{Name: "Data Pipeline", Services: []string{constants.ServicePipeline}},
	}
}

// AllServices returns the distinct services referenced by the given components —
// the set the gateway must probe.
func AllServices(defs []ComponentDef) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range defs {
		for _, s := range d.Services {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Component is a component's rolled-up public status.
type Component struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Healthy int    `json:"healthy_services"`
	Total   int    `json:"total_services"`
}

// Incident is a staff-authored public incident.
type Incident struct {
	ID                 string     `json:"id"`
	Title              string     `json:"title"`
	Body               string     `json:"body,omitempty"`
	Impact             string     `json:"impact"`
	Status             string     `json:"status"`
	AffectedComponents []string   `json:"affected_components"`
	StartedAt          time.Time  `json:"started_at"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
	// CreatedBy (staff JWT subject) is NEVER serialized — /v1/api/status is
	// public, and this would leak internal staff identities to anyone.
	CreatedBy string    `json:"-"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Open reports whether the incident is still unresolved.
func (i Incident) Open() bool { return i.Status != "resolved" }

// Report is the whole public status snapshot.
type Report struct {
	Overall         string      `json:"overall"`      // operational|degraded|major_outage
	OverallText     string      `json:"overall_text"` // human sentence for the banner
	Components      []Component `json:"components"`
	ActiveIncidents []Incident  `json:"active_incidents"`
	RecentIncidents []Incident  `json:"recent_incidents"`
	GeneratedAt     time.Time   `json:"generated_at"`
}

// Rollup is the pure core: given each service's readiness and the incident list,
// it computes per-component status and the overall level. An OPEN incident never
// lets the page read greener than its impact implies (a "major"/"critical" open
// incident forces at least degraded / major_outage even if probes look green —
// staff know something the health check doesn't).
func Rollup(defs []ComponentDef, serviceReady map[string]bool, incidents []Incident, now time.Time) Report {
	rep := Report{Overall: StatusOperational, GeneratedAt: now}
	worst := StatusOperational
	for _, d := range defs {
		healthy, total := 0, len(d.Services)
		for _, s := range d.Services {
			if serviceReady[s] {
				healthy++
			}
		}
		cs := StatusOperational
		switch {
		case total > 0 && healthy == 0:
			cs = StatusDown
		case healthy < total:
			cs = StatusDegraded
		}
		rep.Components = append(rep.Components, Component{Name: d.Name, Status: cs, Healthy: healthy, Total: total})
		if statusRank[cs] > statusRank[worst] {
			worst = cs
		}
	}

	// Fold in open incidents: separate active vs recent, and floor the overall.
	for _, inc := range incidents {
		if inc.Open() {
			rep.ActiveIncidents = append(rep.ActiveIncidents, inc)
			if floor := incidentFloor(inc.Impact); statusRank[floor] > statusRank[worst] {
				worst = floor
			}
		} else {
			rep.RecentIncidents = append(rep.RecentIncidents, inc)
		}
	}

	rep.Overall = worst
	rep.OverallText = overallText(worst, len(rep.ActiveIncidents))
	return rep
}

// incidentFloor maps an incident impact to the minimum overall status it forces.
func incidentFloor(impact string) string {
	switch impact {
	case "critical":
		return StatusDown
	case "major":
		return StatusDegraded
	default:
		return StatusOperational
	}
}

func overallText(level string, activeIncidents int) string {
	switch level {
	case StatusDown:
		return "Major outage"
	case StatusDegraded:
		if activeIncidents > 0 {
			return "Degraded performance — incident in progress"
		}
		return "Degraded performance"
	case StatusMaintenance:
		return "Under maintenance"
	default:
		return "All systems operational"
	}
}

// Store persists incidents (platform-global; no tenant scoping).
type Store interface {
	// RecentIncidents returns open incidents plus resolved ones started within
	// `window`, newest first.
	RecentIncidents(ctx context.Context, window time.Duration, limit int) ([]Incident, error)
	// ListAll returns incidents for the staff console, newest first.
	ListAll(ctx context.Context, limit int) ([]Incident, error)
	// Create inserts a new incident and returns its id.
	Create(ctx context.Context, inc Incident) (string, error)
	// Update mutates an incident's status/impact/body/components; sets resolved_at
	// when status becomes resolved and clears it otherwise. Returns the fresh row.
	Update(ctx context.Context, inc Incident) (*Incident, error)
}
