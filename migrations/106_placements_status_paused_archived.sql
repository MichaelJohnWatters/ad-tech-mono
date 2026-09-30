-- +goose Up
-- Widen the placements status vocabulary to what the app already writes.
--
-- The SSP placement handler (cmd/ssp/management.go updatePlacement) validates and
-- writes 'active' | 'paused' | 'archived' — the publisher-portal Pause/Resume toggle
-- (status='paused') and the archive/soft-delete path (status='archived'). But the
-- original constraint (migration 008) only allowed ('active','inactive'), so pausing
-- a placement hit placements_status_check (23514) → the handler 500'd → the
-- Pause/Resume button silently failed ("nothing happened"). Same gap made the
-- archive/DELETE path 500.
--
-- Safe: serving + the warm cache already read WHERE status='active'
-- (pkg/store/postgres/placements.go), so 'paused'/'archived' correctly drop a
-- placement from serving once the write succeeds. 'inactive' is kept so any legacy
-- row on the old vocabulary is not orphaned.
ALTER TABLE placements DROP CONSTRAINT IF EXISTS placements_status_check;
ALTER TABLE placements ADD CONSTRAINT placements_status_check
    CHECK (status IN ('active', 'paused', 'archived', 'inactive'));

-- +goose Down
ALTER TABLE placements DROP CONSTRAINT IF EXISTS placements_status_check;
ALTER TABLE placements ADD CONSTRAINT placements_status_check
    CHECK (status IN ('active', 'inactive'));
