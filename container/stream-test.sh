#!/usr/bin/env bash
#
# stream-test.sh — listen to Soloist output on localhost:8000
#
# Starts PulseAudio with a null sink, launches Soloist in Connect mode, and
# streams the monitor output as a WAV over HTTP on port 8000. Point ffplay at
# it to hear what Spotify is playing:
#
#   ffplay http://localhost:8000
#
# An embedded Python HTTP server writes a streaming WAV header and pipes raw
# PCM from parec (reading the PulseAudio monitor source) to the client.
#
set -euo pipefail

SOLOIST_API_KEY="${SOLOIST_API_KEY:?SOLOIST_API_KEY is required}"
DATA_DIR="${SOLOIST_DATA_DIR:-/data}"
CACHE_DIR="${SOLOIST_CACHE_DIR:-/cache}"
DEVICE_NAME="${SOLOIST_DEVICE_NAME:-stream-test}"
SAMPLE_RATE=44100
CHANNELS=2
LISTEN_ADDR="${LISTEN_ADDR:-0.0.0.0}"
LISTEN_PORT="${LISTEN_PORT:-8000}"

# PulseAudio needs a runtime dir and home. The parent may pass HOME=/ (common
# under --userns=keep-id), so pin both to controlled scratch dirs we own.
export XDG_RUNTIME_DIR="/tmp/runtime-shim"
export HOME="/tmp/home-shim"
mkdir -p "$XDG_RUNTIME_DIR" "$HOME"
chmod 700 "$XDG_RUNTIME_DIR" "$HOME"

log() { printf '[stream-test] %s\n' "$*" >&2; }

cleanup() {
    log "Cleaning up..."
    jobs -p | xargs -r kill 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT

# --- Start PulseAudio ---
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

TRIES=0
until pactl info >/dev/null 2>&1; do
    TRIES=$((TRIES + 1))
    if [ "$TRIES" -ge 30 ]; then
        log "ERROR: PulseAudio did not become ready" >&2
        exit 1
    fi
    sleep 0.5
done
log "PulseAudio ready"

# --- Start Soloist ---
log "Starting Soloist (Connect mode, --ws 127.0.0.1:0)..."
soloist \
    --device-name "$DEVICE_NAME" \
    --api-key "$SOLOIST_API_KEY" \
    --data-dir "$DATA_DIR" \
    --cache-dir "$CACHE_DIR" \
    --ws 127.0.0.1:0 &

# Wait for ws.port, ignore if it appears late
TRIES=0
until [ -f "${DATA_DIR}/ws.port" ]; do
    if [ "$TRIES" -ge 30 ]; then
        log "WARNING: Soloist did not create ws.port yet; continuing"
        break
    fi
    sleep 0.5
    TRIES=$((TRIES + 1))
done
if [ -f "${DATA_DIR}/ws.port" ]; then
    log "Soloist WebSocket ready at $(cat "${DATA_DIR}/ws.addr"):$(cat "${DATA_DIR}/ws.port")"
    log "Open the Spotify app and select the '${DEVICE_NAME}' device, then choose a track."
    log "  (Alternatively send a play command to the WebSocket described above.)"
else
    log "WARNING: Soloist ws.port unavailable — continuing anyway; playback may not work."
fi

# --- Embedded Python HTTP streaming server ---
log "Starting HTTP server on http://${LISTEN_ADDR}:${LISTEN_PORT}"
log "Listen with: ffplay http://localhost:${LISTEN_PORT}"

exec python3 -u - "$SAMPLE_RATE" "$CHANNELS" "$LISTEN_ADDR" "$LISTEN_PORT" <<'PYEOF'
import http.server
import os
import signal
import subprocess
import sys
import threading

sample_rate = int(sys.argv[1])
channels = int(sys.argv[2])
listen_addr = sys.argv[3]
listen_port = int(sys.argv[4])

# WAV header for s16le PCM. For a stream of unknown length, set both size
# fields to 0xFFFFFFFF (0xFFFFFFFF + 36 overflows 32 bits, so use it raw).
def wav_header(data_size=0xFFFFFFFF):
    byte_rate = sample_rate * channels * 2
    block_align = channels * 2
    hdr = bytearray(44)
    hdr[0:4] = b"RIFF"
    hdr[4:8] = (0xFFFFFFFF).to_bytes(4, "little")
    hdr[8:12] = b"WAVE"
    hdr[12:16] = b"fmt "
    hdr[16:20] = (16).to_bytes(4, "little")
    hdr[20:22] = (1).to_bytes(2, "little")           # PCM
    hdr[22:24] = channels.to_bytes(2, "little")
    hdr[24:28] = sample_rate.to_bytes(4, "little")
    hdr[28:32] = byte_rate.to_bytes(4, "little")
    hdr[32:34] = block_align.to_bytes(2, "little")
    hdr[34:36] = (16).to_bytes(2, "little")          # bits per sample
    hdr[36:40] = b"data"
    hdr[40:44] = data_size.to_bytes(4, "little")
    return hdr

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(wav_header())

        # Start parec writing raw PCM directly to stdout, pipe it out on demand.
        # Each request spawns its own parec so multiple clients work.
        proc = subprocess.Popen(
            [
                "parec",
                "--device=virtual_out.monitor",
                "--format=s16le",
                f"--rate={sample_rate}",
                f"--channels={channels}",
            ],
            stdout=subprocess.PIPE,
        )
        try:
            while True:
                chunk = proc.stdout.read(16384)
                if not chunk:
                    break
                self.wfile.write(chunk)
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            proc.terminate()
            proc.wait()

    def log_message(self, fmt, *args):
        sys.stderr.write("[stream-test:%d] %s\n" % (self.server.server_port, fmt % args))

    def log_request(self, code="-", size="-"):  # quieter
        pass

server = http.server.HTTPServer((listen_addr, listen_port), Handler)
print("HTTP server ready on %s:%d" % (listen_addr, listen_port), flush=True)
server.serve_forever()
PYEOF
