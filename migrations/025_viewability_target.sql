-- +goose Up
-- Viewability guarantee per contract (layer 1: commercial terms).
--
-- Real ad-tech buy/sell contracts often include a viewability guarantee:
-- "publisher commits to ≥70% viewable impressions, or makegood applies."
-- This column records that guarantee on the buyer side (line item) and
-- the seller side (deal). NULL means "no guarantee" — the impression
-- simply gets logged as viewable / not viewable; no contract-level
-- shortfall is computed.
--
-- Values are integer percentages 0-100. We use SMALLINT (2 bytes) rather
-- than REAL because viewability targets are negotiated in whole points
-- ("70%" not "69.4%") and integer comparison is cheaper for the rollup
-- queries that compare delivered viewability vs. target.
--
-- Settlement mechanics (when does spend hit the ledger for vCPM, how
-- long do we hold reservations, what happens on IABViewable=false) are
-- layer 2 — platform engineering decisions that live in code/config,
-- not on the contract row. See docs/PLAN.md → "vCPM Settlement Model
-- (open question)" for the choice that's pending.

ALTER TABLE line_items
    ADD COLUMN viewability_target_pct SMALLINT
        CHECK (viewability_target_pct IS NULL OR
               (viewability_target_pct >= 0 AND viewability_target_pct <= 100));

ALTER TABLE deals
    ADD COLUMN viewability_target_pct SMALLINT
        CHECK (viewability_target_pct IS NULL OR
               (viewability_target_pct >= 0 AND viewability_target_pct <= 100));

-- +goose Down
ALTER TABLE line_items DROP COLUMN viewability_target_pct;
ALTER TABLE deals DROP COLUMN viewability_target_pct;
