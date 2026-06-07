-- +goose Up
-- Advertiser brand domain on creatives.
--
-- This used to be derived at query time by SPLIT_PART(landing_url, '/', 3)
-- — convenient because the YAML's creative_domain field naturally landed
-- as the host part of the rendered landing URL. Worked until commit
-- fb2db9c repointed landing_url at our gateway (/dev/landing/{brand-slug})
-- to host demo landing pages; the host then became "localhost:8080" for
-- every advertiser and the bid responses' ADomain field lost its real
-- brand attribution.
--
-- Fix: store the brand domain explicitly. Seed populates from YAML; the
-- campaigns warm-cache loader reads this column directly instead of
-- parsing it back out of landing_url. Landing URL stays free to point
-- anywhere (gateway proxy in dev, real advertiser site in prod).
--
-- NULL is allowed because (a) older rows pre-date this column and (b)
-- nothing strictly requires a brand domain — the DSP simply omits the
-- ADomain field when this is NULL. Callers that rely on ADomain handle
-- the missing case by falling back to the existing landing_url split.

ALTER TABLE creatives
    ADD COLUMN advertiser_domain TEXT;

-- +goose Down
ALTER TABLE creatives DROP COLUMN advertiser_domain;
