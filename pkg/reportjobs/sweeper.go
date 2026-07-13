package reportjobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/store/objects"
)

// Sweeper enforces artifact retention: expired done/failed jobs lose their
// stored artifact and their row.
type Sweeper struct {
	Store   JobStore
	Objects objects.Store
	Now     func() time.Time
	Log     *slog.Logger
}

// SweepOnce deletes one batch of expired jobs (artifact first, then row) and
// returns how many were removed. A job whose artifact fails to delete keeps
// its row for the next sweep — deleting the row first would orphan the
// object in the bucket forever.
func (s *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	expired, err := s.Store.Expired(ctx, s.Now(), 100)
	if err != nil {
		return 0, fmt.Errorf("list expired: %w", err)
	}
	removed := 0
	for _, j := range expired {
		if j.ArtifactKey != "" {
			exists, err := s.Objects.Exists(ctx, j.ArtifactBucket, j.ArtifactKey)
			if err != nil {
				s.Log.Error("sweep: artifact check failed", "job", j.ID, "key", j.ArtifactKey, "error", err)
				continue
			}
			if exists {
				if err := s.Objects.Delete(ctx, j.ArtifactBucket, j.ArtifactKey); err != nil {
					s.Log.Error("sweep: artifact delete failed", "job", j.ID, "key", j.ArtifactKey, "error", err)
					continue
				}
			}
		}
		if err := s.Store.Delete(ctx, j.ID); err != nil {
			s.Log.Error("sweep: job delete failed", "job", j.ID, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		s.Log.Info("report jobs swept", "removed", removed)
	}
	return removed, nil
}
