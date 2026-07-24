-- +goose Up
-- IAB Audience Taxonomy 1.1 reference table + the optional standard-taxonomy
-- label on audience segments.
--
-- Why: segments are named freeform per tenant, which is fine for the tenant's
-- own targeting but meaningless to anyone else. Labelling a segment with an
-- IAB Audience Taxonomy node id is what lets the SSP express it to EXTERNAL
-- buyers as OpenRTB user.data with ext.segtax=4 — the id becomes
-- interpretable outside the platform. The label is optional; unlabelled
-- segments keep working exactly as before and never ride user.data.
--
-- Global reference data: no account_id, no RLS (same precedent as
-- config_schema — a platform-wide lookup table, not tenant state). `path` is
-- the full breadcrumb ("Interest | Automotive | Auto Body Styles | SUV") that
-- pickers display and fuzzy-search over.
--
-- The seed below is a DEMO SUBSET shaped like the official taxonomy,
-- sufficient for local dev, the portal picker, and e2e. The numeric ids are
-- stable platform-local stand-ins, NOT certified against the official IAB
-- Tech Lab file — for real cross-party interop, reload this table from the
-- official Audience Taxonomy 1.1 TSV (free licence, iabtechlab.com), keeping
-- the official Unique IDs.

CREATE TABLE iab_audience_taxonomy (
    id        BIGINT PRIMARY KEY,
    parent_id BIGINT REFERENCES iab_audience_taxonomy(id),
    name      TEXT NOT NULL,
    path      TEXT NOT NULL
);

INSERT INTO iab_audience_taxonomy (id, parent_id, name, path) VALUES
  (1,   NULL, 'Demographic',            'Demographic'),
  (2,   1,    'Age Range',              'Demographic | Age Range'),
  (3,   2,    '18-24',                  'Demographic | Age Range | 18-24'),
  (4,   2,    '25-34',                  'Demographic | Age Range | 25-34'),
  (5,   2,    '35-44',                  'Demographic | Age Range | 35-44'),
  (6,   2,    '45-54',                  'Demographic | Age Range | 45-54'),
  (7,   2,    '55+',                    'Demographic | Age Range | 55+'),
  (8,   1,    'Household Income',       'Demographic | Household Income'),
  (10,  NULL, 'Interest',               'Interest'),
  (11,  10,   'Automotive',             'Interest | Automotive'),
  (12,  11,   'Auto Body Styles',       'Interest | Automotive | Auto Body Styles'),
  (13,  12,   'SUV',                    'Interest | Automotive | Auto Body Styles | SUV'),
  (14,  12,   'Sedan',                  'Interest | Automotive | Auto Body Styles | Sedan'),
  (15,  12,   'Pickup Truck',           'Interest | Automotive | Auto Body Styles | Pickup Truck'),
  (20,  10,   'Sports',                 'Interest | Sports'),
  (21,  20,   'Running & Jogging',      'Interest | Sports | Running & Jogging'),
  (22,  20,   'Soccer',                 'Interest | Sports | Soccer'),
  (23,  20,   'Golf',                   'Interest | Sports | Golf'),
  (30,  10,   'Technology & Computing', 'Interest | Technology & Computing'),
  (31,  30,   'Consumer Electronics',   'Interest | Technology & Computing | Consumer Electronics'),
  (32,  30,   'Artificial Intelligence','Interest | Technology & Computing | Artificial Intelligence'),
  (40,  10,   'Travel',                 'Interest | Travel'),
  (41,  40,   'Air Travel',             'Interest | Travel | Air Travel'),
  (42,  40,   'Hotels & Motels',        'Interest | Travel | Hotels & Motels'),
  (45,  10,   'Food & Drink',           'Interest | Food & Drink'),
  (50,  10,   'Style & Fashion',        'Interest | Style & Fashion'),
  (100, NULL, 'Purchase Intent',        'Purchase Intent'),
  (101, 100,  'Automotive',             'Purchase Intent | Automotive'),
  (102, 101,  'In-Market SUV',          'Purchase Intent | Automotive | In-Market SUV'),
  (103, 101,  'In-Market Electric Vehicle', 'Purchase Intent | Automotive | In-Market Electric Vehicle'),
  (110, 100,  'Consumer Electronics',   'Purchase Intent | Consumer Electronics'),
  (115, 100,  'Travel Bookings',        'Purchase Intent | Travel Bookings'),
  (120, 100,  'Financial Products',     'Purchase Intent | Financial Products'),
  (121, 120,  'Credit Cards',           'Purchase Intent | Financial Products | Credit Cards');

-- The label itself: nullable — NULL means "custom classification only", the
-- default for every existing and new segment.
ALTER TABLE audience_segments
    ADD COLUMN taxonomy_id BIGINT REFERENCES iab_audience_taxonomy(id);

-- The SSP's hot-path map query filters on (visibility, taxonomy_id NOT NULL).
CREATE INDEX idx_segments_taxonomy ON audience_segments (taxonomy_id)
    WHERE taxonomy_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_segments_taxonomy;
ALTER TABLE audience_segments DROP COLUMN IF EXISTS taxonomy_id;
DROP TABLE iab_audience_taxonomy;
