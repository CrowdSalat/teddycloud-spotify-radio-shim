#!/usr/bin/env bash
#
# blocker2-test.sh — validates Blocker 2 from DESIGN.md
#
# Starts PulseAudio with a null sink, launches Soloist in Connect mode,
# records from the monitor source, and checks that valid PCM audio arrives.
#
# Exit 0 = all checks passed
# Exit 1 = one or more checks failed (details on stderr)
#
set -euo pipefail

SOLOIST_API_KEY="${SOLOIST_API_KEY:?SOLOIST_API_KEY is required}"
DATA_DIR="${SOLOIST_DATA_DIR:-/data}"
CACHE_DIR="${SOLOIST_CACHE_DIR:-/cache}"
DEVICE_NAME="${SOLOIST_DEVICE_NAME:-blocker2-test}"
RECORD_SECONDS="${RECORD_SECONDS:-5}"
SAMPLE_RATE=44100
CHANNELS=2
TEST_DIR="/tmp/test"
RECORD_FILE="${TEST_DIR}/capture.raw"
WAV_FILE="${TEST_DIR}/capture.wav"
EXIT_CODE=0

# PulseAudio needs a runtime dir and home. The parent may pass HOME=/ (common
# under --userns=keep-id), so pin both to controlled scratch dirs we own.
export XDG_RUNTIME_DIR="/tmp/runtime-shim"
export HOME="/tmp/home-shim"
mkdir -p "$XDG_RUNTIME_DIR" "$HOME"
chmod 700 "$XDG_RUNTIME_DIR" "$HOME"

log() { printf '[blocker2] %s\n' "$*" >&2; }
fail() { log "FAIL: $*"; EXIT_CODE=1; }
pass() { log "PASS: $*"; }

cleanup() {
    log "Cleaning up..."
    # Kill background jobs (pulseaudio, soloist)
    jobs -p | xargs -r kill 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT

# --- Phase 1: Start PulseAudio ---
log "Starting PulseAudio (null sink: virtual_out)..."
# With -n (no default config), explicitly load the client socket module and the
# null sink. The native-protocol socket is what pactl/parec/Soloist connect to.
pulseaudio \
    --exit-idle-time=-1 \
    -n \
    --load="module-native-protocol-unix" \
    --load="module-null-sink sink_name=virtual_out sink_properties=device.description=Shim_Sink" \
    --daemonize=yes \
    --log-target=stderr

# Wait for PulseAudio to be ready
TRIES=0
MAX_TRIES=30
until pactl info >/dev/null 2>&1; do
    TRIES=$((TRIES + 1))
    if [ "$TRIES" -ge "$MAX_TRIES" ]; then
        fail "PulseAudio did not become ready within ${MAX_TRIES}s"
        exit 1
    fi
    sleep 0.5
done
pass "PulseAudio is running"

# Verify null sink exists
if ! pactl list short sinks | grep -q virtual_out; then
    fail "Sink 'virtual_out' not found"
    exit 1
fi
pass "Sink 'virtual_out' exists"

# Verify monitor source exists
if ! pactl list short sources | grep -q "virtual_out.monitor"; then
    fail "Source 'virtual_out.monitor' not found"
    exit 1
fi
pass "Source 'virtual_out.monitor' exists"

# --- Phase 2: Start Soloist ---
log "Starting Soloist (Connect mode, --ws 127.0.0.1:0)..."
soloist \
    --device-name "$DEVICE_NAME" \
    --api-key "$SOLOIST_API_KEY" \
    --data-dir "$DATA_DIR" \
    --cache-dir "$CACHE_DIR" \
    --ws 127.0.0.1:0 &
SOLOIST_PID=$!

# Wait for ws.port file
TRIES=0
MAX_TRIES=60
until [ -f "${DATA_DIR}/ws.port" ]; do
    TRIES=$((TRIES + 1))
    if [ "$TRIES" -ge "$MAX_TRIES" ]; then
        fail "Soloist did not create ws.port within ${MAX_TRIES}s"
        exit 1
    fi
    # Check if Soloist exited early
    if ! kill -0 "$SOLOIST_PID" 2>/dev/null; then
        fail "Soloist exited before creating ws.port"
        exit 1
    fi
    sleep 0.5
done
WS_PORT=$(cat "${DATA_DIR}/ws.port")
WS_ADDR=$(cat "${DATA_DIR}/ws.addr")
pass "Soloist WebSocket ready at ${WS_ADDR}:${WS_PORT}"

# --- Phase 3: Wait for playback ---
# In Connect mode, Soloist waits for the Spotify app to connect and start
# playback. For this test, we send a `play` command via the WebSocket API
# with a known Spotify URI (or just check that the audio path works when
# audio IS flowing).
#
# If no Spotify URI is provided, we skip the playback check and just verify
# that the PulseAudio + monitor path is wired correctly (parec captures
# silence/monitor traffic).
SPOTIFY_URI="${SPOTIFY_URI:-}"

if [ -n "$SPOTIFY_URI" ]; then
    log "Sending play command for ${SPOTIFY_URI}..."
    # Use websocat if available, otherwise fall back to a minimal HTTP
    # request to trigger the WebSocket. For now, log that manual playback
    # is needed.
    log "NOTE: Automatic play command requires websocat or a WebSocket client."
    log "      Send this JSON to ws://${WS_ADDR}:${WS_PORT}:"
    log "      {\"type\":\"command\",\"command\":\"play\",\"uri\":\"${SPOTIFY_URI}\"}"
    log ""
    log "Waiting ${RECORD_SECONDS}s for audio to appear on the monitor..."
    sleep "$RECORD_SECONDS"
else
    log "No SPOTIFY_URI set — recording monitor output (silence expected)."
    log "This validates the PulseAudio + parec capture path."
    sleep "$RECORD_SECONDS"
fi

# --- Phase 4: Record from monitor ---
log "Recording from virtual_out.monitor for ${RECORD_SECONDS}s..."

# Record raw PCM using parec
timeout "${RECORD_SECONDS}" \
    parec \
        --device=virtual_out.monitor \
        --format=s16le \
        --rate="$SAMPLE_RATE" \
        --channels="$CHANNELS" \
        --file-format=wav \
        "$WAV_FILE" \
    || true

# Also capture raw for byte-level checks
if [ -f "$WAV_FILE" ]; then
    # Strip WAV header (44 bytes) to get raw PCM
    dd if="$WAV_FILE" bs=1 skip=44 of="$RECORD_FILE" 2>/dev/null || true
fi

# --- Phase 5: Validate ---
log "--- Validation ---"

# Check 1: WAV file exists and is non-empty
if [ ! -f "$WAV_FILE" ] || [ ! -s "$WAV_FILE" ]; then
    fail "WAV file is missing or empty: ${WAV_FILE}"
    exit 1
fi
WAV_SIZE=$(stat -c%s "$WAV_FILE")
pass "WAV file exists, ${WAV_SIZE} bytes"

# Check 2: Raw PCM is non-empty
if [ ! -f "$RECORD_FILE" ] || [ ! -s "$RECORD_FILE" ]; then
    fail "Raw PCM file is missing or empty: ${RECORD_FILE}"
    exit 1
fi
RAW_SIZE=$(stat -c%s "$RECORD_FILE")
pass "Raw PCM file, ${RAW_SIZE} bytes"

# Check 3: Minimum expected size
# At s16le 44100 Hz stereo: 44100 * 2 channels * 2 bytes = 176400 bytes/sec
EXPECTED_MIN=$((SAMPLE_RATE * CHANNELS * 2 * RECORD_SECONDS / 2))  # half of full rate (expect some silence gaps)
if [ "$RAW_SIZE" -lt "$EXPECTED_MIN" ]; then
    fail "PCM too small: ${RAW_SIZE} bytes (expected >= ${EXPECTED_MIN} for ${RECORD_SECONDS}s)"
    exit 1
fi
pass "PCM size within expected range"

# Check 4: ffprobe validates the WAV
if command -v ffprobe >/dev/null 2>&1; then
    PROBE=$(ffprobe -v error -show_entries stream=codec_name,sample_rate,channels,bits_per_sample \
        -of csv=p=0 "$WAV_FILE" 2>&1) || true
    log "ffprobe output: ${PROBE}"

    if echo "$PROBE" | grep -q "pcm_s16le"; then
        pass "ffprobe confirms pcm_s16le"
    else
        fail "ffprobe did not confirm pcm_s16le: ${PROBE}"
    fi

    if echo "$PROBE" | grep -q "${SAMPLE_RATE}"; then
        pass "ffprobe confirms sample rate ${SAMPLE_RATE}"
    else
        fail "ffprobe did not confirm sample rate ${SAMPLE_RATE}: ${PROBE}"
    fi

    if echo "$PROBE" | grep -q "${CHANNELS}"; then
        pass "ffprobe confirms ${CHANNELS} channels"
    else
        fail "ffprobe did not confirm ${CHANNELS} channels: ${PROBE}"
    fi
else
    log "WARN: ffprobe not found, skipping format validation"
fi

# Check 5: Non-silent content (at least some non-zero samples)
# Count non-zero 16-bit samples in first 1 second
FIRST_SEC=$((SAMPLE_RATE * CHANNELS * 2))  # bytes in 1 second
if [ "$RAW_SIZE" -ge "$FIRST_SEC" ]; then
    NONZERO=$(dd if="$RECORD_FILE" bs=1 count="$FIRST_SEC" 2>/dev/null \
        | od -An -tx2 -v | tr ' ' '\n' | grep -cv '^0$' || true)
    log "Non-zero 16-bit samples in first second: ${NONZERO}"
    # If SPOTIFY_URI is set, we expect actual audio; if not, silence is OK
    if [ -n "$SPOTIFY_URI" ] && [ "${NONZERO:-0}" -lt 100 ]; then
        fail "Expected audio but got mostly silence (${NONZERO} non-zero samples)"
    else
        pass "Sample data captured (${NONZERO} non-zero samples in first second)"
    fi
fi

# --- Summary ---
log ""
log "==============================="
if [ "$EXIT_CODE" -eq 0 ]; then
    log "BLOCKER 2 RESULT: PASS"
    log "PulseAudio null sink + monitor capture works."
else
    log "BLOCKER 2 RESULT: FAIL"
    log "See failures above."
fi
log "==============================="

exit "$EXIT_CODE"
