package keys

import (
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"
)

var contentPackagerSet = config.NewKeySet("content-packager")

// ContentPackagerSchema is the ContentPackager schema — passed to config.Setup at boot.
func ContentPackagerSchema() []config.SchemaEntry {
	return append(contentPackagerSet.Entries(), transcodeSharedEntries()...)
}

// ContentPackager holds the ContentPackager config keys.
var ContentPackager = struct {
	PackagerSourceBucket   config.StringKey
	PackagerSourceKey      config.StringKey
	PackagerContentID      config.StringKey
	PackagerPrefix         config.StringKey
	PackagerBreakAtSegment config.IntKey
	PackagerBreakSegments  config.IntKey
	PackagerAudio          config.BoolKey
	PackagerContainer      config.StringKey
	PackagerTargetDuration config.IntKey
	PackagerBreaks         config.StringKey
}{
	PackagerSourceBucket:   contentPackagerSet.String("packager.source_bucket", "adtech-creatives", config.TierStatic, "Object-store bucket holding the source content MP4 and receiving the packaged HLS.", config.Since("v1.6")),
	PackagerSourceKey:      contentPackagerSet.String("packager.source_key", "media/bbb-720-10mb.mp4", config.TierStatic, "Object key of the source content MP4 to package.", config.Since("v1.6")),
	PackagerContentID:      contentPackagerSet.String("packager.content_id", "sample", config.TierStatic, "Content id — the packaged HLS lands at {prefix}/{content_id}/index.m3u8.", config.Since("v1.6")),
	PackagerPrefix:         contentPackagerSet.String("packager.prefix", "ssai/content", config.TierStatic, "Object-key prefix for the packaged content HLS.", config.Since("v1.6")),
	PackagerBreakAtSegment: contentPackagerSet.Int("packager.break_at_segment", "2", config.TierStatic, "Segment index where the mid-roll ad break opens (#EXT-X-CUE-OUT).", config.Since("v1.6")),
	PackagerBreakSegments:  contentPackagerSet.Int("packager.break_segments", "5", config.TierStatic, "Number of content segments the ad break spans (replaced by the stitched ad).", config.Since("v1.6")),
	PackagerAudio:          contentPackagerSet.Bool("packager.audio", "false", config.TierStatic, "Package a single audio-only rendition ({prefix}/{content_id}/audio/index.m3u8, no master) instead of the video ABR ladder — for audio SSAI origins.", config.Since("v1.6")),
	PackagerContainer:      contentPackagerSet.String("packager.container", "ts", config.TierStatic, "Segment container: 'ts' (MPEG-TS, HLS-only) or 'cmaf' (fMP4 .m4s + init.mp4, shared by HLS and DASH). Use cmaf for DASH SSAI origins.", config.Since("v1.6")),
	PackagerTargetDuration: contentPackagerSet.Int("packager.target_duration_sec", "0", config.TierStatic, "If >0, loop the source MP4 up to this many seconds before packaging (turns a short clip into a longer content stream with room for multiple ad breaks). 0 = use the source as-is.", config.Since("v2.5")),
	PackagerBreaks:         contentPackagerSet.String("packager.breaks", "", config.TierStatic, "Comma-separated ad-break positions to stamp: 'pre' (start), 'mid' (middle), 'post' (near end) — e.g. 'pre,mid,post'. Empty = a single break at break_at_segment/break_segments (legacy).", config.Since("v2.5")),
}
