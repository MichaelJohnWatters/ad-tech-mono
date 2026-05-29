package identity

import (
	"testing"
	"time"
)

func TestGraph_LinkAndResolve(t *testing.T) {
	g := NewGraph()

	// Two platform IDs with the same hashed email = same person
	g.Link("pid-1", Signal{Type: "hashed_email", Value: "sha256_abc", Confidence: 1.0})
	g.Link("pid-2", Signal{Type: "hashed_email", Value: "sha256_abc", Confidence: 1.0})

	ids := g.Resolve("pid-1")
	if len(ids) != 2 {
		t.Errorf("expected 2 linked IDs, got %d", len(ids))
	}

	ids2 := g.Resolve("pid-2")
	if len(ids2) != 2 {
		t.Errorf("expected 2 linked IDs from pid-2, got %d", len(ids2))
	}
}

func TestGraph_NoLink(t *testing.T) {
	g := NewGraph()

	g.Link("pid-1", Signal{Type: "hashed_email", Value: "sha256_abc"})
	g.Link("pid-2", Signal{Type: "hashed_email", Value: "sha256_def"})

	ids := g.Resolve("pid-1")
	if len(ids) != 1 {
		t.Errorf("expected 1 ID (no link), got %d", len(ids))
	}
}

func TestGraph_CrossDevice(t *testing.T) {
	g := NewGraph()

	// Desktop
	g.Link("pid-desktop", Signal{Type: "hashed_email", Value: "sha256_user1", Source: "pub-a"})
	// Mobile - same hashed email
	g.Link("pid-mobile", Signal{Type: "hashed_email", Value: "sha256_user1", Source: "pub-b"})

	ids := g.Resolve("pid-desktop")
	if len(ids) != 2 {
		t.Errorf("expected cross-device link, got %d IDs", len(ids))
	}
}

func TestGraph_Delete(t *testing.T) {
	g := NewGraph()

	g.Link("pid-1", Signal{Type: "hashed_email", Value: "sha256_abc"})
	g.Link("pid-2", Signal{Type: "hashed_email", Value: "sha256_abc"})

	g.Delete("pid-1")

	profile := g.Profile("pid-1")
	if profile != nil {
		t.Error("expected nil profile after deletion")
	}

	// pid-2 should still exist but no longer linked to pid-1
	ids := g.Resolve("pid-2")
	if len(ids) != 1 || ids[0] != "pid-2" {
		t.Errorf("expected only pid-2 after deletion, got %v", ids)
	}
}

func TestGraph_Profile(t *testing.T) {
	g := NewGraph()

	g.Link("pid-1", Signal{Type: "hashed_email", Value: "sha256_abc", Timestamp: time.Now()})
	g.Link("pid-1", Signal{Type: "publisher_user_id", Value: "pub_user_123", Source: "pub-a"})

	profile := g.Profile("pid-1")
	if profile == nil {
		t.Fatal("expected profile")
	}
	if len(profile.Signals) != 2 {
		t.Errorf("expected 2 signals, got %d", len(profile.Signals))
	}
}

func TestGeneratePlatformID(t *testing.T) {
	id1 := GeneratePlatformID()
	id2 := GeneratePlatformID()
	if id1 == id2 {
		t.Error("expected unique IDs")
	}
	if len(id1) < 10 {
		t.Errorf("ID too short: %s", id1)
	}
}

func TestGraph_Stats(t *testing.T) {
	g := NewGraph()
	g.Link("pid-1", Signal{Type: "email", Value: "a"})
	g.Link("pid-2", Signal{Type: "email", Value: "a"})
	g.Link("pid-3", Signal{Type: "email", Value: "b"})

	stats := g.Stats()
	if stats.TotalProfiles != 3 {
		t.Errorf("profiles = %d, want 3", stats.TotalProfiles)
	}
}
