#!/usr/bin/env bash
#
# gen-ad-creative.sh — regenerate cmd/seed/assets/ad-video.mp4, the "AD" card used
# as the demo VIDEO ad creative. It's a bold "AD" card (big white AD on red) so a
# served pre-roll/SSAI ad is VISUALLY DISTINCT from the Big Buck Bunny content clip
# — previously both the content and the video creatives were bbb, so a played ad
# looked identical to content.
#
# This ffmpeg build has no drawtext/libfreetype and there's no ImageMagick, so the
# card is rendered to a PNG in Go (golang.org/x/image, in a throwaway /tmp module so
# the repo go.mod is untouched), then looped into a tiny h264 mp4.
#
# Requires: ffmpeg, Go, and /System/Library/Fonts/Supplemental/Arial.ttf (macOS).
set -euo pipefail
cd "$(dirname "$0")/.."
OUT="cmd/seed/assets/ad-video.mp4"
FONT="${AD_FONT:-/System/Library/Fonts/Supplemental/Arial.ttf}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
[ -f "$FONT" ] || { echo "font not found: $FONT (set AD_FONT)"; exit 1; }

cat > "$TMP/main.go" <<GO
package main

import (
	"image"; "image/color"; "image/draw"; "image/png"; "log"; "os"
	"golang.org/x/image/font"; "golang.org/x/image/font/opentype"; "golang.org/x/image/math/fixed"
)

func main() {
	const W, H = 640, 360
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{0xC4, 0x24, 0x24, 0xFF}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, 96, W, 250), &image.Uniform{color.RGBA{0, 0, 0, 0x33}}, image.Point{}, draw.Over)
	fb, err := os.ReadFile(os.Args[1]); if err != nil { log.Fatal(err) }
	ft, err := opentype.Parse(fb); if err != nil { log.Fatal(err) }
	text(img, ft, "AD", 200, W/2, 232, color.White)
	text(img, ft, "ADVERTISEMENT", 34, W/2, 300, color.RGBA{0xFF, 0xD1, 0x66, 0xFF})
	text(img, ft, "ad-tech-mono demo creative", 18, W/2, 338, color.RGBA{0xFF, 0xFF, 0xFF, 0xDD})
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
  GOFLAGS=-mod=mod go get golang.org/x/image/font/opentype@v0.41.0 >/dev/null 2>&1
  GOFLAGS=-mod=mod go run . "$FONT" "$TMP/ad-card.png" )

ffmpeg -y -loop 1 -i "$TMP/ad-card.png" -t 8 -r 25 \
  -c:v libx264 -pix_fmt yuv420p -profile:v baseline -level 3.0 -movflags +faststart -an "$OUT" >/dev/null 2>&1

echo "✔ wrote $OUT ($(wc -c < "$OUT" | tr -d ' ') bytes) — reseed to publish (uploaded to Minio as media/ad-video.mp4)"
