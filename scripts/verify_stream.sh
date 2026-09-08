#!/bin/sh
# Measure a reostream endpoint against the camera's configured frame rate.
#
# usage: verify_stream.sh <address> <stream> <seconds> [username] [password]
#
# The assertion that matters is frames divided by seconds. A raw elementary
# stream carries no timing, so a muxer that invents timestamps produces a frame
# count wildly out of step with wall clock. That is the failure this exists to
# catch, and it is not subtle when it happens: a previous tool produced a
# 216fps stream from a 20fps camera.
#
# Decode errors should be zero. Note that a handful at connect time are
# expected until keyframe aligned join is implemented: a client joining mid GOP
# sees slices before the next parameter sets and complains until it resynks.
# Those stop once the first keyframe arrives. Errors that continue past the
# first second are real.
#
# Do not point this at a stream that Frigate is currently using. A camera
# permits one Baichuan connection per stream, so a second client gets a
# session that never delivers frames, and the process holding it blocks its own
# replacement until the camera times the session out. Check first:
#   ssh root@<nvr host> 'docker exec frigate sh -c "ps -e -o args= | grep [n]eolink-cat"'

set -e

ADDR="$1"
STREAM="${2:-sub}"
SECS="${3:-120}"
USER="${4:-admin}"
PASS="${5:-}"

if [ -z "$ADDR" ]; then
  echo "usage: $0 <address> <stream> <seconds> [username] [password]" >&2
  exit 2
fi

BIN=${REOSTREAM_BIN:-./reostream}
PORT=${REOSTREAM_PORT:-8599}
NAME=verify
TMP=$(mktemp -d)
trap 'kill -TERM "$RS" 2>/dev/null; rm -rf "$TMP"' EXIT INT TERM

"$BIN" -address "$ADDR" -username "$USER" -password "$PASS" \
       -stream "$STREAM" -name "$NAME" -listen "127.0.0.1:$PORT" \
       > "$TMP/reostream.log" 2>&1 &
RS=$!

# Wait for the endpoint rather than sleeping a guessed interval.
i=0
while [ "$i" -lt 60 ]; do
  if curl -s --max-time 1 -o /dev/null "http://127.0.0.1:$PORT/$NAME.ts"; then
    break
  fi
  i=$((i + 1))
done

if ! kill -0 "$RS" 2>/dev/null; then
  echo "reostream exited during startup:" >&2
  cat "$TMP/reostream.log" >&2
  exit 1
fi

ffmpeg -hide_banner -loglevel warning \
       -i "http://127.0.0.1:$PORT/$NAME.ts" -t "$SECS" -c copy \
       -f mp4 -y "$TMP/capture.mp4" 2> "$TMP/ffmpeg.err"

FRAMES=$(ffprobe -v error -count_frames -select_streams v:0 \
         -show_entries stream=nb_read_frames -of csv=p=0 "$TMP/capture.mp4")
DURATION=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$TMP/capture.mp4")
CODEC=$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of csv=p=0 "$TMP/capture.mp4")
SIZE=$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0 "$TMP/capture.mp4")
ERRORS=$(grep -cE "error|corrupt|non-existing|no frame" "$TMP/ffmpeg.err" || true)
RSS=$(ps -o rss= -p "$RS" 2>/dev/null | tr -d ' ')

echo "address    $ADDR $STREAM"
echo "codec      $CODEC $SIZE"
echo "frames     $FRAMES"
echo "duration   $DURATION"
echo "fps        $(awk "BEGIN{printf \"%.2f\", $FRAMES/$DURATION}")"
echo "errors     $ERRORS"
echo "rss        ${RSS:-gone} kB"

if [ "$ERRORS" -gt 0 ]; then
  echo
  echo "ffmpeg messages:"
  sed 's/^/  /' "$TMP/ffmpeg.err"
fi
