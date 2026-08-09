package main

import (
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/idgen"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/simulator/request"
	"gopkg.in/yaml.v3"
)

// simInv is the process-wide seeded placement pool. Loaded once at the start of
// the run/single commands; nil means "not loaded" → callers use the built-in
// simulator placement. The run loop is single-goroutine, so no locking.
var simInv *inventory

// initInventory loads the seeded placements and logs the spread. Safe to call
// with an unreadable dir — the inventory is simply empty and the simulator
// falls back to its built-in placement.
func initInventory(dir string, log *slog.Logger) {
	simInv = loadInventory(dir)
	log.Info("simulator inventory loaded",
		"dir", dir, "placements", simInv.count(),
		"display", len(simInv.byChannel[request.Display]),
		"video", len(simInv.byChannel[request.Video]),
		"native", len(simInv.byChannel[request.Native]),
		"audio", len(simInv.byChannel[request.Audio]),
	)
}

// invPick returns a random seeded placement for the channel, or ok=false when
// the inventory is unloaded/empty for that channel.
func invPick(ch request.Channel, rng *rand.Rand) (invPlacement, bool) {
	if simInv == nil {
		return invPlacement{}, false
	}
	return simInv.pick(ch, rng)
}

// This file lets the simulator spread traffic across every seeded publisher +
// placement instead of hammering the single "Publisher Simulator" placement.
// It reads the SAME profiles/publishers/*.yaml that cmd/seed loads, so the
// placement keys it sends resolve to real rows in the SSP's warm cache. A
// per-request pick means every seeded publisher shows realistic supply-side
// activity (requests / fill / earnings) in its portal.

// invPlacement is one seeded placement the simulator can target. The mirror
// path only needs Key (the SSP resolves publisher/floor/categories from it);
// PublisherID/TagID/Floor are for the --direct OpenRTB path.
type invPlacement struct {
	Key         string
	Name        string
	Domain      string
	PublisherID string // idgen UUID — matches how seed derives it
	TagID       string // placement UUID
	Floor       float64
	Categories  []string
}

// inventory groups the seeded placements by channel so a pick respects format.
type inventory struct {
	byChannel map[request.Channel][]invPlacement
}

// --- minimal mirror of the seed YAML (only the fields we target on) ---

type invProfileYAML struct {
	Publishers []struct {
		ID         string `yaml:"id"`
		Name       string `yaml:"name"`
		Domain     string `yaml:"domain"`
		Placements []struct {
			ID         string   `yaml:"id"`
			Name       string   `yaml:"name"`
			Format     string   `yaml:"format"`
			FloorPrice float64  `yaml:"floor_price"`
			Categories []string `yaml:"categories"`
		} `yaml:"placements"`
	} `yaml:"publishers"`
}

// formatChannel maps a placement's YAML format to the simulator channel.
func formatChannel(format string) (request.Channel, bool) {
	switch format {
	case "display", "":
		return request.Display, true
	case "video":
		return request.Video, true
	case "native":
		return request.Native, true
	case "audio":
		return request.Audio, true
	default:
		return "", false
	}
}

// loadInventory reads every YAML in dir and returns the placements grouped by
// channel. On any error it returns an empty inventory — callers fall back to
// the built-in simulator placement, so the simulator still runs outside the
// repo (where the profiles dir isn't present).
func loadInventory(dir string) *inventory {
	inv := &inventory{byChannel: map[request.Channel][]invPlacement{}}
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return inv
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var prof invProfileYAML
		if err := yaml.Unmarshal(data, &prof); err != nil {
			continue
		}
		for _, pub := range prof.Publishers {
			for _, pl := range pub.Placements {
				ch, ok := formatChannel(pl.Format)
				if !ok {
					continue
				}
				inv.byChannel[ch] = append(inv.byChannel[ch], invPlacement{
					Key:         pl.ID,
					Name:        pl.Name,
					Domain:      pub.Domain,
					PublisherID: idgen.Derive("publisher", pub.ID),
					TagID:       idgen.Derive("placement", pl.ID),
					Floor:       pl.FloorPrice,
					Categories:  pl.Categories,
				})
			}
		}
	}
	return inv
}

// pick returns a random seeded placement for the channel. ok=false when the
// inventory has none for that channel (e.g. native/audio only exist on the
// simulator publisher), so the caller uses the built-in placement instead.
func (inv *inventory) pick(ch request.Channel, rng *rand.Rand) (invPlacement, bool) {
	pool := inv.byChannel[ch]
	if len(pool) == 0 {
		return invPlacement{}, false
	}
	return pool[rng.Intn(len(pool))], true
}

// pickByInterest returns a random placement for the channel whose categories
// intersect the persona's interests — a dog person lands on dog pages. ok=false
// when no placement matches (caller falls back to the unbiased pick).
func (inv *inventory) pickByInterest(ch request.Channel, interests []string, rng *rand.Rand) (invPlacement, bool) {
	if len(interests) == 0 {
		return invPlacement{}, false
	}
	var matches []invPlacement
	for _, pl := range inv.byChannel[ch] {
		if categoriesIntersect(pl.Categories, interests) {
			matches = append(matches, pl)
		}
	}
	if len(matches) == 0 {
		return invPlacement{}, false
	}
	return matches[rng.Intn(len(matches))], true
}

func categoriesIntersect(cats, interests []string) bool {
	for _, c := range cats {
		for _, i := range interests {
			if c == i {
				return true
			}
		}
	}
	return false
}

// invPickThemed is the interest-affine variant of invPick.
func invPickThemed(ch request.Channel, interests []string, rng *rand.Rand) (invPlacement, bool) {
	if simInv == nil {
		return invPlacement{}, false
	}
	return simInv.pickByInterest(ch, interests, rng)
}

// count returns how many placements the inventory holds across all channels.
func (inv *inventory) count() int {
	n := 0
	for _, pool := range inv.byChannel {
		n += len(pool)
	}
	return n
}
