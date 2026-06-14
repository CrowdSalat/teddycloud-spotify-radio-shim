#!/usr/bin/env bash
# Integration tests for teddycloud-spotify-radio-shim.
# Runs container-based acceptance tests for each implemented phase.
#
# Usage:
#   ./scripts/test-integration.sh [--config-dir PATH] [--skip-build]
#
#   --config-dir PATH   Path to a directory containing go-librespot's
#                       state.json (Spotify session). Required for Phase 3
#                       and 4 tests. Obtain by authenticating once and
#                       copying /config out of the running container.
#   --skip-build        Skip image build (use existing shim-integration-test).
#
# Example (with auth):
#   ./scripts/test-integration.sh --config-dir /private/tmp/shim-config

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

IMAGE="shim-integration-test"
CONFIG_DIR=""
SKIP_BUILD=0
PASS=0
FAIL=0

# Temp files cleaned up on exit.
TMP_AUDIO=$(mktemp)
TMP_SWAP=$(mktemp)
trap 'rm -f "$TMP_AUDIO" "$TMP_SWAP"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

pass() { echo "  ✅ $1"; PASS=$((PASS + 1)); }
fail() { echo "  ❌ $1"; FAIL=$((FAIL + 1)); }

section() { echo ""; echo "=== $1 ==="; }

start_container() {
    local name=$1 port=$2
    podman rm -f "$name" 2>/dev/null || true
    local args=(-d --name "$name" --platform linux/amd64 -p "${port}:8080")
    if [ -n "$CONFIG_DIR" ]; then
        args+=(-v "${CONFIG_DIR}:/config:Z")
    fi
    podman run "${args[@]}" "$IMAGE" >/dev/null
}

stop_container() {
    podman rm -f "$1" 2>/dev/null || true
}

wait_for_log() {
    local name=$1 pattern=$2 timeout=${3:-20}
    for _ in $(seq 1 "$timeout"); do
        if podman logs "$name" 2>&1 | grep -q "$pattern"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case $1 in
        --config-dir) CONFIG_DIR="$2"; shift 2 ;;
        --skip-build) SKIP_BUILD=1; shift ;;
        *) echo "Unknown argument: $1" >&2; exit 1 ;;
    esac
done

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

if [ "$SKIP_BUILD" -eq 0 ]; then
    section "Building image"
    cd "$ROOT_DIR"
    podman build --platform linux/amd64 -f Containerfile -t "$IMAGE" . 2>&1 | tail -3
else
    echo "(skipping build)"
fi

# ---------------------------------------------------------------------------
# Phase 1 — Subprocess Lifecycle
# ---------------------------------------------------------------------------

section "Phase 1 — Subprocess Lifecycle"
C="shim-it-phase1"
start_container "$C" 8091

if wait_for_log "$C" "go-librespot is ready"; then
    pass "go-librespot starts"
else
    fail "go-librespot did not reach ready state"
fi

if podman exec "$C" bash -c 'echo > /dev/tcp/localhost/3678' 2>/dev/null; then
    pass "port 3678 reachable"
else
    fail "port 3678 not reachable"
fi

OLD_PID=$(podman exec "$C" pgrep -x go-librespot 2>/dev/null | head -1 || echo "")
if [ -n "$OLD_PID" ]; then
    podman exec "$C" kill -9 "$OLD_PID" 2>/dev/null || true
    sleep 5
    NEW_PID=$(podman exec "$C" pgrep -x go-librespot 2>/dev/null | head -1 || echo "")
    if [ -n "$NEW_PID" ] && [ "$NEW_PID" != "$OLD_PID" ]; then
        pass "go-librespot restarted after crash (PID $OLD_PID → $NEW_PID)"
    else
        fail "go-librespot did not restart after crash"
    fi
else
    fail "could not get go-librespot PID"
fi

podman stop -t 5 "$C" >/dev/null
STATE=$(podman inspect "$C" --format '{{.State.Status}}' 2>/dev/null)
[ "$STATE" = "exited" ] && pass "clean shutdown" || fail "unclean shutdown (state=$STATE)"
stop_container "$C"

# ---------------------------------------------------------------------------
# Phase 3 & 4 — require Spotify auth
# ---------------------------------------------------------------------------

if [ -z "$CONFIG_DIR" ]; then
    echo ""
    echo "⚠️  --config-dir not provided — skipping Phase 3 and 4 (require Spotify auth)"
    echo "   Authenticate once, then run:"
    echo "   $0 --config-dir /path/to/config"
else
    # -----------------------------------------------------------------------
    # Phase 3 — /stream endpoint
    # -----------------------------------------------------------------------

    section "Phase 3 — /stream endpoint"
    C="shim-it-phase3"
    start_container "$C" 8093

    if wait_for_log "$C" "go-librespot is ready"; then
        pass "container started"
    else
        fail "container failed to start"
        stop_container "$C"
    fi

    RESULT=$(curl -s -o "$TMP_AUDIO" \
        -w "%{http_code}|%{content_type}|%{size_download}" \
        --max-time 15 \
        "http://localhost:8093/stream?spotify_uri=spotify:track:4PTG3Z6ehGkBFwjybzWkR8" \
        2>/dev/null || echo "000||0")

    HTTP_CODE=$(echo "$RESULT" | cut -d'|' -f1)
    CONTENT_TYPE=$(echo "$RESULT" | cut -d'|' -f2)
    BYTES=$(echo "$RESULT" | cut -d'|' -f3 | tr -d ' ')

    [ "$HTTP_CODE" = "200" ] \
        && pass "HTTP 200" \
        || fail "HTTP $HTTP_CODE (want 200)"

    [ "$CONTENT_TYPE" = "audio/wav" ] \
        && pass "Content-Type: audio/wav" \
        || fail "Content-Type: $CONTENT_TYPE (want audio/wav)"

    { [ -n "$BYTES" ] && [ "$BYTES" -gt 0 ]; } 2>/dev/null \
        && pass "audio bytes received ($BYTES)" \
        || fail "no audio bytes received"

    if AUDIO_FILE="$TMP_AUDIO" python3 - <<'PYEOF' 2>/dev/null; then
import struct, os
d = open(os.environ['AUDIO_FILE'], 'rb').read(44)
assert d[:4] == b'RIFF' and d[8:12] == b'WAVE'
assert struct.unpack_from('<I', d, 4)[0] == 0xFFFFFFFF
PYEOF
        pass "valid streaming WAV header (RIFF, WAVE, size=0xFFFFFFFF)"
    else
        fail "invalid WAV header"
    fi

    stop_container "$C"

    # -----------------------------------------------------------------------
    # Phase 4 — Hot-Swap
    # -----------------------------------------------------------------------

    section "Phase 4 — Hot-Swap"
    C="shim-it-phase4"
    start_container "$C" 8094

    if wait_for_log "$C" "go-librespot is ready"; then
        pass "container started"
    else
        fail "container failed to start"
        stop_container "$C"
    fi

    # Start first stream.
    curl -s -N -o /dev/null \
        "http://localhost:8094/stream?spotify_uri=spotify:track:4PTG3Z6ehGkBFwjybzWkR8" &
    CURL1=$!
    sleep 5

    kill -0 $CURL1 2>/dev/null \
        && pass "first stream running" \
        || fail "first stream died before hot-swap"

    # Trigger hot-swap.
    curl -s -o "$TMP_SWAP" --max-time 8 \
        "http://localhost:8094/stream?spotify_uri=spotify:track:6rqhFgbbKwnb9MLmUQDhG6" \
        2>/dev/null || true

    kill -0 $CURL1 2>/dev/null \
        && fail "first stream still alive after hot-swap" \
        || pass "first stream killed by hot-swap"

    SWAP_BYTES=$(wc -c < "$TMP_SWAP" 2>/dev/null | tr -d ' ' || echo 0)
    { [ -n "$SWAP_BYTES" ] && [ "$SWAP_BYTES" -gt 0 ]; } 2>/dev/null \
        && pass "second stream received data ($SWAP_BYTES bytes)" \
        || fail "second stream got no data"

    stop_container "$C"
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo ""
echo "=============================="
printf " Results: %d passed, %d failed\n" "$PASS" "$FAIL"
echo "=============================="
echo ""

[ "$FAIL" -eq 0 ] && exit 0 || exit 1
