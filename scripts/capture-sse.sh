#!/usr/bin/env bash
# Capture raw Teddycloud SSE events with timestamps for Phase 6 discovery.
#
# Usage:
#   ./scripts/capture-sse.sh [SSE_URL] [OUT_FILE]
#
# Defaults: SSE_URL=http://localhost:8080/api/sse, OUT_FILE=docs/research/teddycloud-sse-capture.txt
#
# Requires an active port-forward:
#   oc port-forward svc/teddycloud 8080:80 -n app-teddycloud
#
# Events are appended to OUT_FILE as they arrive. A blank line separates events
# in the SSE protocol; each captured event is prefixed with a timestamp so the
# operator's actions can be correlated later.

set -euo pipefail

SSE_URL="${1:-http://localhost:8080/api/sse}"
OUT_FILE="${2:-$(dirname "$0")/../docs/research/teddycloud-sse-capture.txt}"

mkdir -p "$(dirname "$OUT_FILE")"

echo "== capture started $(date -Is) from $SSE_URL ==" >> "$OUT_FILE"

event=""
emit() {
    printf '\n== %s ==\n%s\n' "$(date -Is)" "$event" >> "$OUT_FILE"
    event=""
}

curl -sN --max-time 86400 "$SSE_URL" | while IFS= read -r line; do
    if [ -z "$line" ]; then
        emit
    else
        event="${event}${line}"$'\n'
    fi
done