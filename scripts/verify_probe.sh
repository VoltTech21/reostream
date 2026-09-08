#!/usr/bin/env bash
# Phase 0 gate: does our own protocol implementation produce a decodable stream?
set -euo pipefail
CAM="${1:?usage: verify_probe.sh <camera-ip> [stream] [seconds]}"
STREAM="${2:-sub}"
SECS="${3:-30}"
OUT=/tmp/bcprobe_verify.h264

echo "--- streaming ${SECS}s of ${STREAM} from ${CAM} ---"
./bcprobe --address "$CAM" --stream "$STREAM" --seconds "$SECS" > "$OUT"
ls -l "$OUT"

echo "--- ffprobe ---"
ffprobe -v error -count_frames \
    -show_entries stream=codec_name,width,height,nb_read_frames \
    -of default=noprint_wrappers=1 "$OUT"

echo "--- decode errors ---"
ERRS=$(ffmpeg -v error -i "$OUT" -f null - 2>&1 | wc -l)
echo "decode error lines: $ERRS"
[ "$ERRS" -eq 0 ] || echo "WARNING: decoder reported errors"
