-- +goose Up
-- Publisher channel inventory: allow placements to declare the emerging channels
-- (retail sponsored slots, in-game scene surfaces) alongside the existing set,
-- and a surface/slot count for them. DOOH was already permitted.
ALTER TABLE placements DROP CONSTRAINT IF EXISTS placements_format_check;
ALTER TABLE placements ADD CONSTRAINT placements_format_check
    CHECK (format IN ('display', 'native', 'video', 'audio', 'dooh', 'retail', 'ingame'));
ALTER TABLE placements ADD COLUMN surfaces INT;

-- +goose Down
ALTER TABLE placements DROP COLUMN surfaces;
ALTER TABLE placements DROP CONSTRAINT IF EXISTS placements_format_check;
ALTER TABLE placements ADD CONSTRAINT placements_format_check
    CHECK (format IN ('display', 'native', 'video', 'audio', 'dooh'));
