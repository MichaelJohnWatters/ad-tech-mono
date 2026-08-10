package statuspage

import (
	"testing"
	"time"
)

func defs() []ComponentDef {
	return []ComponentDef{
		{Name: "A", Services: []string{"s1", "s2"}},
		{Name: "B", Services: []string{"s3"}},
	}
}

func TestRollupAllOperational(t *testing.T) {
	ready := map[string]bool{"s1": true, "s2": true, "s3": true}
	rep := Rollup(defs(), ready, nil, time.Now())
	if rep.Overall != StatusOperational {
		t.Errorf("overall = %q, want operational", rep.Overall)
	}
	for _, c := range rep.Components {
		if c.Status != StatusOperational {
			t.Errorf("component %s = %q, want operational", c.Name, c.Status)
		}
	}
}

func TestRollupPartialIsDegraded(t *testing.T) {
	ready := map[string]bool{"s1": true, "s2": false, "s3": true}
	rep := Rollup(defs(), ready, nil, time.Now())
	if rep.Overall != StatusDegraded {
		t.Errorf("overall = %q, want degraded", rep.Overall)
	}
}

func TestRollupAllDownComponentIsMajorOutage(t *testing.T) {
	ready := map[string]bool{"s1": false, "s2": false, "s3": true}
	rep := Rollup(defs(), ready, nil, time.Now())
	if rep.Components[0].Status != StatusDown {
		t.Errorf("component A = %q, want major_outage", rep.Components[0].Status)
	}
	if rep.Overall != StatusDown {
		t.Errorf("overall = %q, want major_outage", rep.Overall)
	}
}

func TestRollupOpenIncidentFloorsOverall(t *testing.T) {
	// Everything probes green, but an open critical incident forces major outage.
	ready := map[string]bool{"s1": true, "s2": true, "s3": true}
	incs := []Incident{{Title: "DB down", Impact: "critical", Status: "investigating"}}
	rep := Rollup(defs(), ready, incs, time.Now())
	if rep.Overall != StatusDown {
		t.Errorf("overall = %q, want major_outage (open critical incident)", rep.Overall)
	}
	if len(rep.ActiveIncidents) != 1 {
		t.Errorf("active incidents = %d, want 1", len(rep.ActiveIncidents))
	}
}

func TestRollupResolvedIncidentIsRecentNotActive(t *testing.T) {
	ready := map[string]bool{"s1": true, "s2": true, "s3": true}
	incs := []Incident{{Title: "old", Impact: "major", Status: "resolved"}}
	rep := Rollup(defs(), ready, incs, time.Now())
	if rep.Overall != StatusOperational {
		t.Errorf("overall = %q, want operational (resolved incident must not floor)", rep.Overall)
	}
	if len(rep.ActiveIncidents) != 0 || len(rep.RecentIncidents) != 1 {
		t.Errorf("active=%d recent=%d, want 0/1", len(rep.ActiveIncidents), len(rep.RecentIncidents))
	}
}

func TestAllServicesDistinct(t *testing.T) {
	got := AllServices([]ComponentDef{
		{Name: "A", Services: []string{"x", "y"}},
		{Name: "B", Services: []string{"y", "z"}},
	})
	if len(got) != 3 {
		t.Errorf("distinct services = %v, want 3", got)
	}
}
