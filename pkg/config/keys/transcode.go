package keys

import "github.com/MichaelJohnWatters/ad-tech-mono/pkg/config"

// transcodeShared holds keys deliberately registered by MORE THAN ONE binary
// (ssai, content-packager, prewarm each seed transcode.ladder for their own
// pods). The service label is blank — config.Setup fills in the registering
// binary's name — so the key is still declared exactly once.
var transcodeSharedSet = config.NewKeySet("")

func transcodeSharedEntries() []config.SchemaEntry { return transcodeSharedSet.Entries() }

// Transcode holds the shared transcode-pipeline keys.
var Transcode = struct {
	Ladder config.StringKey
}{
	Ladder: transcodeSharedSet.String("transcode.ladder", "", config.TierLive, "ABR ladder spec: comma-separated WxH@vbitrateKbps rungs (e.g. 640x360@800,854x480@1400,1280x720@2800), each inheriting the default codec/fps/audio. Empty = built-in 360/480/720p default. Shared by packager/prewarm/stitcher — re-run the content-packager after changing so origins match.", config.Since("v1.6")),
}
