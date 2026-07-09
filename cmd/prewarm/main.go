// cmd/prewarm is a periodic Job that conditions every active video/audio ad
// creative across the ABR ladder ahead of time, so the SSAI serving path is a
// cache hit (no cold-transcode latency / slate). Complements the on-miss async
// warm the stitcher already does — this warms BEFORE first serve.
//
// See docs/SSAI_CONDITIONING.md (P6).
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	_ "github.com/lib/pq"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/constants"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/logger"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/routes"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/transcode"
)

var log = logger.New("prewarm")

var schema = []config.SchemaEntry{
	{Key: "prewarm.transcoder_url", Type: "string", Tier: config.TierStatic, Default: routes.DefaultTranscoderURL, Description: "Transcoder the prewarm job conditions creatives against.", Service: constants.ServiceTranscoder, Since: "v1.6"},
}

func main() {
	sc := config.Setup(constants.ServiceTranscoder, schema, log)
	cfg := sc.Cfg
	ctx := context.Background()

	transcoderURL := cfg.Get("prewarm.transcoder_url", routes.DefaultTranscoderURL)
	dbURL := cfg.Get("database.url", cfg.Get("database_url", ""))
	if dbURL == "" {
		log.Error("database url not set")
		os.Exit(1)
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Error("db open", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	creatives, err := activeMediaCreatives(ctx, db)
	if err != nil {
		log.Error("query creatives", "error", err)
		os.Exit(1)
	}
	client := &http.Client{Timeout: 6 * time.Minute}

	warmed, failed := 0, 0
	for _, c := range creatives {
		// Audio creatives condition to a single audio-only profile; video
		// creatives condition across the whole ABR ladder.
		profiles := transcode.DefaultLadder()
		if c.format == "audio" {
			profiles = []transcode.Profile{transcode.DefaultAudioProfile()}
		}
		for _, p := range profiles {
			if err := condition(ctx, client, transcoderURL, c.id, c.mediaURL, p); err != nil {
				log.Warn("prewarm condition failed", "creative", c.id, "format", c.format, "profile", p.Hash(), "error", err)
				failed++
				continue
			}
			warmed++
		}
	}
	log.Info("prewarm complete", "creatives", len(creatives), "warmed", warmed, "failed", failed)
}

type creative struct {
	id       string
	format   string
	mediaURL string
}

// activeMediaCreatives returns video/audio creatives with a media URL. NOTE: the
// creatives table is RLS-protected; run this job with a DB role granted
// cross-tenant read (or BYPASSRLS), else it sees only the connection's tenant.
func activeMediaCreatives(ctx context.Context, db *sql.DB) ([]creative, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id::text, format, asset_url
		FROM creatives
		WHERE format IN ('video','audio')
		  AND asset_url IS NOT NULL AND asset_url <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []creative
	for rows.Next() {
		var c creative
		if err := rows.Scan(&c.id, &c.format, &c.mediaURL); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func condition(ctx context.Context, client *http.Client, transcoderURL, creativeID, mediaURL string, p transcode.Profile) error {
	body, _ := json.Marshal(map[string]any{"creative_id": creativeID, "media_url": mediaURL, "profile": p})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, transcoderURL+routes.TranscodeCondition, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("transcoder status %d", resp.StatusCode)
	}
	return nil
}
