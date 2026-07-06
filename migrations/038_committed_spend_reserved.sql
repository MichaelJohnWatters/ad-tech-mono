-- +goose Up

-- Persist the open-reserve portion of committed spend alongside settled, so a
-- reporting restart restores in-flight reserves too — not just settled. Without
-- this, reserves open at the restart instant vanish from committed, the next
-- snapshot publishes low, and DSPs reconcile down (a brief overspend window for
-- those reserves). On boot the reserved total is restored as one synthetic
-- per-campaign hold with a fresh TTL; a pre-restart reserve that settles after
-- boot is a bounded, conservative over-count the sweep clears within one hold
-- TTL. CPM campaigns have no reserves, so this only matters for CPC/CPA/vCPM.
ALTER TABLE campaign_committed_spend
    ADD COLUMN reserved_cents BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE campaign_committed_spend
    DROP COLUMN reserved_cents;
