#!/usr/bin/env bash
#
# gen-brand-ad-videos.sh — regenerate the PER-ADVERTISER video ad creatives in
# cmd/seed/assets/ad-video-<slug>.mp4. Each is a brand-coloured card with the
# advertiser's NAME + domain + an "ADVERTISEMENT" tag, so when an ad is stitched
# into a video/SSAI stream you can SEE which advertiser won the auction (before
# this, every video line item shared one generic "AD" card and all ads looked
# identical). The generic cmd/seed/assets/ad-video.mp4 stays as the fallback —
# see gen-ad-creative.sh.
#
# Same technique as gen-ad-creative.sh: this ffmpeg build has no drawtext, so the
# card is rendered to a PNG in Go (golang.org/x/image, throwaway /tmp module) then
# looped into a tiny baseline-h264 mp4. Reseed to publish (media.go embeds +
# uploads each as media/ad-video-<slug>.mp4; the DSP profiles point each video
# line item at its brand's key).
#
# Requires: ffmpeg, Go, and an Arial.ttf (macOS path by default; set AD_FONT).
set -euo pipefail
cd "$(dirname "$0")/.."
FONT="${AD_FONT:-/System/Library/Fonts/Supplemental/Arial.ttf}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
[ -f "$FONT" ] || { echo "font not found: $FONT (set AD_FONT)"; exit 1; }

# slug | display name | domain | bg R G B | accent R G B
BRANDS=(
  "globex|Globex Tech|globex-tech.com|26 86 219|125 211 252"
  "cloudcrm|CloudCRM|cloudcrm.io|15 118 110|153 246 228"
  "luxauto|LuxAuto|luxauto.com|17 24 39|234 179 8"
  "quickbite|QuickBite|quickbite.app|234 88 12|255 237 213"
  "epicquest|Epic Quest|epicquest.game|109 40 217|221 214 254"
)

cat > "$TMP/main.go" <<'GO'
package main

import (
	"image"; "image/color"; "image/draw"; "image/png"; "log"; "os"; "strconv"
	"golang.org/x/image/font"; "golang.org/x/image/font/opentype"; "golang.org/x/image/math/fixed"
)

// args: font out.png name domain bgR bgG bgB acR acG acB
func main() {
	const W, H = 640, 360
	atoi := func(i int) uint8 { n, _ := strconv.Atoi(os.Args[i]); return uint8(n) }
	bg := color.RGBA{atoi(5), atoi(6), atoi(7), 0xFF}
	ac := color.RGBA{atoi(8), atoi(9), atoi(10), 0xFF}
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	// Darkening band behind the name for contrast on any bg.
	draw.Draw(img, image.Rect(0, 120, W, 232), &image.Uniform{color.RGBA{0, 0, 0, 0x3A}}, image.Point{}, draw.Over)
	// Top-left "AD" chip so it reads as an ad even muted / mid-scroll.
	draw.Draw(img, image.Rect(24, 24, 92, 62), &image.Uniform{ac}, image.Point{}, draw.Over)
	fb, err := os.ReadFile(os.Args[1]); if err != nil { log.Fatal(err) }
	ft, err := opentype.Parse(fb); if err != nil { log.Fatal(err) }
	text(img, ft, "AD", 28, 58, 54, bg)                                                   // chip label
	text(img, ft, os.Args[3], 60, W/2, 196, color.White)                                  // brand name
	text(img, ft, os.Args[4], 26, W/2, 262, ac)                                           // domain
	text(img, ft, "ADVERTISEMENT", 20, W/2, 300, color.RGBA{0xFF, 0xFF, 0xFF, 0xCC})      // ad tag
	text(img, ft, "ad-tech-mono demo creative", 15, W/2, 336, color.RGBA{0xFF, 0xFF, 0xFF, 0x99})
	f, err := os.Create(os.Args[2]); if err != nil { log.Fatal(err) }
	defer f.Close()
	if err := png.Encode(f, img); err != nil { log.Fatal(err) }
}

func text(img *image.RGBA, ft *opentype.Font, s string, size float64, cx, baselineY int, col color.Color) {
	face, err := opentype.NewFace(ft, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull}); if err != nil { log.Fatal(err) }
	d := &font.Drawer{Dst: img, Src: image.NewUniform(col), Face: face}
	d.Dot = fixed.Point26_6{X: fixed.I(cx) - d.MeasureString(s)/2, Y: fixed.I(baselineY)}
	d.DrawString(s)
}
GO

( cd "$TMP"
  go mod init adgen >/dev/null 2>&1
  GOFLAGS=-mod=mod go get golang.org/x/image/font/opentype@v0.41.0 >/dev/null 2>&1 )

for b in "${BRANDS[@]}"; do
  IFS='|' read -r slug name domain bg ac <<< "$b"
  read -r bgR bgG bgB <<< "$bg"
  read -r acR acG acB <<< "$ac"
  png="$TMP/$slug.png"
  out="cmd/seed/assets/ad-video-$slug.mp4"
  ( cd "$TMP" && GOFLAGS=-mod=mod go run . "$FONT" "$png" "$name" "$domain" "$bgR" "$bgG" "$bgB" "$acR" "$acG" "$acB" )
  ffmpeg -y -loop 1 -i "$png" -t 8 -r 25 \
    -c:v libx264 -pix_fmt yuv420p -profile:v baseline -level 3.0 -movflags +faststart -an "$out" >/dev/null 2>&1
  echo "✔ $out ($(wc -c < "$out" | tr -d ' ') bytes) — $name / $domain"
done
echo "reseed to publish (media.go embeds + uploads each as media/ad-video-<slug>.mp4)"
