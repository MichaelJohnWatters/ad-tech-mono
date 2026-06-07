package postgres

import (
	"testing"
)

func TestParseCreativesJSON(t *testing.T) {
	t.Run("empty string returns nil", func(t *testing.T) {
		if got := parseCreativesJSON(""); got != nil {
			t.Errorf("expected nil for empty string, got %+v", got)
		}
	})
	t.Run("empty array returns nil", func(t *testing.T) {
		if got := parseCreativesJSON("[]"); got != nil {
			t.Errorf("expected nil for [], got %+v", got)
		}
	})
	t.Run("single display entry", func(t *testing.T) {
		raw := `[{"id":"abc-123","fmt":"display","w":300,"h":250,"dur":0,"media":""}]`
		out := parseCreativesJSON(raw)
		if len(out) != 1 {
			t.Fatalf("expected 1, got %d", len(out))
		}
		if out[0].ID != "abc-123" || out[0].Width != 300 || out[0].Height != 250 || out[0].Format != "display" {
			t.Errorf("decoded shape wrong: %+v", out[0])
		}
	})
	t.Run("video entry carries duration + media URL", func(t *testing.T) {
		raw := `[{"id":"vid-1","fmt":"video","w":640,"h":360,"dur":15,"media":"https://cdn/x.mp4"}]`
		out := parseCreativesJSON(raw)
		if len(out) != 1 {
			t.Fatalf("expected 1, got %d", len(out))
		}
		v := out[0]
		if v.Format != "video" || v.Duration != 15 || v.MediaURL != "https://cdn/x.mp4" {
			t.Errorf("video fields lost: %+v", v)
		}
	})
	t.Run("multiple sizes preserve order", func(t *testing.T) {
		// Loader query orders by line_item_creatives.weight DESC; we
		// just trust the input order here. Test pins that we don't
		// reorder inside parseCreativesJSON.
		raw := `[{"id":"a","fmt":"display","w":300,"h":250},{"id":"b","fmt":"display","w":728,"h":90},{"id":"c","fmt":"display","w":300,"h":600}]`
		out := parseCreativesJSON(raw)
		if len(out) != 3 {
			t.Fatalf("expected 3, got %d", len(out))
		}
		wantIDs := []string{"a", "b", "c"}
		for i, want := range wantIDs {
			if out[i].ID != want {
				t.Errorf("order drift at %d: got %q, want %q", i, out[i].ID, want)
			}
		}
		if out[1].Width != 728 || out[1].Height != 90 {
			t.Errorf("[1] dims wrong: %+v", out[1])
		}
	})
	t.Run("malformed JSON returns nil", func(t *testing.T) {
		// Defensive: bad input must not panic. The loader silently drops
		// the creative list rather than failing the whole campaign — the
		// campaign still loads with the legacy CreativeID populated.
		if got := parseCreativesJSON("{not json"); got != nil {
			t.Errorf("expected nil for malformed JSON, got %+v", got)
		}
	})
}
