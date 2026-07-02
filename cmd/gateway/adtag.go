package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/middleware"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
)

// placementTag is the metadata the tag generator needs about a placement.
type placementTag struct {
	ID     string
	Name   string
	Format string
	Width  int
	Height int
}

type adTagStore interface {
	// GetPlacementForTag returns the placement iff it belongs to accountID;
	// sql.ErrNoRows otherwise (unknown or cross-tenant, indistinguishable to the
	// caller by design).
	GetPlacementForTag(ctx context.Context, accountID, placementID string) (placementTag, error)
}

// adTagResponse is the generated embed snippet plus the metadata a UI needs to
// render a copy box and a preview.
type adTagResponse struct {
	PlacementID string `json:"placement_id"`
	Format      string `json:"format"`
	TagType     string `json:"tag_type"` // js | prebid | vast
	Tag         string `json:"tag"`
}

// adTagHandler generates a publisher embed snippet for a placement (GET,
// placements:read). Tenant-scoped — the placement must belong to the caller's
// account. Read-only: it derives the snippet from the placement row and the
// route constants, writing nothing.
//
// tag_type selects the shape:
//   - js     → adtech.js slot div + init + requestAd (display/native)
//   - prebid → a Prebid ad-unit config stub
//   - vast   → a VAST tag URL for the video/audio player
func adTagHandler(store adTagStore, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims := middleware.ClaimsFromContext(r.Context())
		if claims == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !can(claims, "placements:read") {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		placementID := strings.TrimSpace(r.URL.Query().Get("placement_id"))
		if placementID == "" {
			http.Error(w, `{"error":"placement_id query param required"}`, http.StatusBadRequest)
			return
		}
		tagType := r.URL.Query().Get("tag_type")
		if tagType == "" {
			tagType = "js"
		}
		if tagType != "js" && tagType != "prebid" && tagType != "vast" {
			http.Error(w, `{"error":"tag_type must be js, prebid or vast"}`, http.StatusBadRequest)
			return
		}

		p, err := store.GetPlacementForTag(r.Context(), claims.AccountID, placementID)
		if err == sql.ErrNoRows {
			http.Error(w, `{"error":"placement not found"}`, http.StatusNotFound)
			return
		}
		if err != nil {
			log.Error("adtag placement lookup failed", "error", err)
			http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
			return
		}

		_ = json.NewEncoder(w).Encode(adTagResponse{
			PlacementID: p.ID,
			Format:      p.Format,
			TagType:     tagType,
			Tag:         renderAdTag(tagType, p),
		})
	}
}

// renderAdTag builds the embed snippet for one placement + tag type. It mirrors
// the real adtech.js SDK contract (web/static/adtech.js) and the pubad routes so
// a pasted tag actually resolves against this stack.
func renderAdTag(tagType string, p placementTag) string {
	slotID := "adtech-slot-" + shortID(p.ID)
	switch tagType {
	case "prebid":
		size := fmt.Sprintf("[%d, %d]", p.Width, p.Height)
		return fmt.Sprintf(`pbjs.addAdUnits([{
  code: %q,
  mediaTypes: { banner: { sizes: [%s] } },
  bids: [{ bidder: 'adtechmono', params: { placementId: %q } }]
}]);`, slotID, size, p.ID)
	case "vast":
		// A VAST tag URL the player (IMA / video.js) fetches per ad request.
		return fmt.Sprintf(`%s?placement_id=%s`, routes.PublisherAdServeVAST, p.ID)
	default: // js
		return fmt.Sprintf(`<div id=%q style="width:%dpx;height:%dpx"></div>
<script src="/static/adtech.js"></script>
<script>
  adtech.init({ debug: false });
  adtech.requestAd({ placementId: %q, elementId: %q });
</script>`, slotID, p.Width, p.Height, p.ID, slotID)
	}
}

// shortID returns the first UUID segment (or the whole string if it has no
// dash) — enough to make a slot id unique on a page without leaking the full id.
func shortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

type pgAdTagStore struct{ db *sql.DB }

func (s pgAdTagStore) GetPlacementForTag(ctx context.Context, accountID, placementID string) (placementTag, error) {
	var p placementTag
	if s.db == nil {
		return p, sql.ErrConnDone
	}
	var width, height sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text, name, format, width, height FROM placements
		 WHERE id = $1::uuid AND account_id = $2::uuid`, placementID, accountID).
		Scan(&p.ID, &p.Name, &p.Format, &width, &height)
	if err != nil {
		return p, err
	}
	p.Width, p.Height = int(width.Int64), int(height.Int64)
	return p, nil
}
