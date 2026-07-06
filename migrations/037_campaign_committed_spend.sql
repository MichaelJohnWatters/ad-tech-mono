-- +goose Up

-- Campaign committed-spend snapshot — durable backing for the DSP pacing
-- reconciliation loop.
--
-- The billing engine tracks per-campaign COMMITTED spend (settled-today +
-- open reserves) in an in-process accumulator and broadcasts it so DSPs
-- reconcile their pacing counters to billed reality. That accumulator is
-- volatile: a reporting restart would reset committed to zero, the next
-- snapshot would publish a low number, and every DSP would reconcile its
-- budget counter DOWN — effectively handing campaigns back budget they had
-- already spent (overspend). This table persists the SETTLED portion (the
-- realized-spend part) each snapshot tick so a restart re-hydrates it. Open
-- reserves are transient (they settle or expire within the pacing hold TTL)
-- and are intentionally NOT persisted — they rebuild from live events.
--
-- Billing-internal (like reservation_context / ledger_entries): written and
-- read by the reporting service, keyed by (day, campaign), never per-tenant,
-- so no account_id / RLS. No FK to line_items — persistence must not fail
-- because a campaign was archived mid-day. Keyed by day so it self-expires
-- logically at the UTC boundary (old rows are ignored on load; prune offline).
CREATE TABLE campaign_committed_spend (
    day           DATE NOT NULL,
    campaign_id   TEXT NOT NULL,
    settled_cents BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (day, campaign_id)
);

-- +goose Down
DROP TABLE campaign_committed_spend;
