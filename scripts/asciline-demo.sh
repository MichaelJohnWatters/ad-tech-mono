#!/usr/bin/env bash
# Ad-free ASCILINE reference testbed (see docs/PLAN.md -> "Ideas — could do").
#
# Runs the upstream ASCILINE engine (github.com/YusufB5/ASCILINE) against a
# locally generated test clip so we can observe the frame pipeline we'd need
# to replicate for our own Go text-video channel: binary WebSocket frames,
# RAW/zlib/delta codec behaviour, Canvas rendering cost, bandwidth per mode.
#
# LICENSE GUARDRAIL: ASCILINE is MIT *with an anti-advertisement clause* —
# using it to serve, deliver, or display ads (even simulated, even locally)
# terminates the license. This demo streams plain test content only. Do NOT
# point it at the SSAI stitcher, SSP, or anything on the ad path.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIR="$ROOT/third_party/asciline"
CLIP="$ROOT/third_party/asciline-sample.mp4"

for bin in git python3 ffmpeg; do
    command -v "$bin" >/dev/null || { echo "error: $bin required (ffmpeg is the same prereq as make ssai-smoke)"; exit 1; }
done

if [ ! -d "$DIR" ]; then
    echo "==> cloning ASCILINE into third_party/ (gitignored)"
    git clone --depth 1 https://github.com/YusufB5/ASCILINE "$DIR"
fi

[ -x "$DIR/.venv/bin/python" ] || python3 -m venv "$DIR/.venv"
# Always re-run: no-op in ~1s when satisfied, resumes a previously failed install.
echo "==> ensuring deps (fastapi/uvicorn/opencv/numpy/websockets)"
"$DIR/.venv/bin/pip" install --quiet fastapi uvicorn opencv-python numpy websockets

# Deterministic ad-free sample: 30s test pattern + 440Hz tone. Regenerate by
# deleting the file; swap in any mp4 via ASCILINE_CLIP=/path/to/video.mp4.
CLIP="${ASCILINE_CLIP:-$CLIP}"
if [ ! -f "$CLIP" ]; then
    echo "==> generating sample clip (ffmpeg testsrc2, 640x360@30, 30s)"
    ffmpeg -hide_banner -loglevel error -y \
        -f lavfi -i "testsrc2=size=640x360:rate=30:duration=30" \
        -f lavfi -i "sine=frequency=440:duration=30" \
        -c:v libx264 -pix_fmt yuv420p -c:a aac "$CLIP"
fi

echo "==> ASCILINE demo (content only, no ads) -> http://localhost:8000"
cd "$DIR"
# stream_server runs uvicorn in a daemon thread; the main thread is an
# interactive input() console. Under Tilt / background (non-TTY stdin) that
# input() hits EOF and the whole process dies, so keep stdin open forever.
if [ ! -t 0 ]; then
    exec .venv/bin/python stream_server.py "$CLIP" --cols 240 "$@" < <(tail -f /dev/null)
fi
exec .venv/bin/python stream_server.py "$CLIP" --cols 240 "$@"
