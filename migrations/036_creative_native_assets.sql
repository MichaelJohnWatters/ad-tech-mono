-- +goose Up
-- Native creatives carry a set of typed assets (title, images, sponsored-by,
-- body, CTA, landing url) rather than a single banner image or video file.
-- Store them as one JSONB blob on the creative; shape mirrors
-- pkg/native.AssetSet / models.NativeAssets. Nullable so existing display/
-- video/audio creatives need no backfill. creatives is already tenant-scoped
-- with an RLS policy, so no new policy is required for an added column.
ALTER TABLE creatives ADD COLUMN native_assets JSONB;

-- +goose Down
ALTER TABLE creatives DROP COLUMN native_assets;
