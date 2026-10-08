#!/usr/bin/env bash
# Start cmd/mock-toniebox as the simulated Toniebox.
#
# Invoked by `make bench-box`. A script for the same reason as bench-shim.sh: an
# inline `setsid … &` in a make recipe wedges make, because the recipe shell
# keeps the child's inherited descriptors open.

set -euo pipefail

: "${BENCH_DIR:=$(pwd)/.bench}"
: "${BENCH_ADDR:=http://localhost:9095}"
: "${BENCH_WEB:=http://localhost:8080}"
: "${BENCH_TC_API:=https://localhost:8443}"
: "${BENCH_RUID:=E00403500EEA4BF2}"

die() {
	printf 'bench-box: %s\n' "$*" >&2
	exit 1
}

if curl -fsS -m 2 -o /dev/null "$BENCH_ADDR/" 2>/dev/null; then
	printf 'box: already listening on %s\n' "$BENCH_ADDR"
	exit 0
fi

if ss -ltn 2>/dev/null | grep -q ':9095'; then
	die "port 9095 is busy but not answering — a stale bench?
       run './scripts/cycle.sh down' or 'make bench-stop' first"
fi

mkdir -p "$BENCH_DIR/logs"

# The redirection is on the subshell rather than the command inside it: a
# backgrounded subshell inherits this script's stdout, which under `make` is
# make's pipe, so make would block until the pipe closes. See bench-shim.sh.
(
	cd "$BENCH_DIR" || exit 1
	exec setsid ./mock-toniebox \
		--teddycloud "$BENCH_TC_API" \
		--sse "$BENCH_WEB" \
		--addr :9095 \
		--uid "$BENCH_RUID"
) >"$BENCH_DIR/logs/box.log" 2>&1 </dev/null &

for _ in $(seq 1 15); do
	if curl -fsS -m 1 -o /dev/null "$BENCH_ADDR/" 2>/dev/null; then
		printf 'box: %s (figurine %s)\n' "$BENCH_ADDR" "$BENCH_RUID"
		printf '     log: .bench/logs/box.log\n'
		exit 0
	fi
	sleep 1
done

printf 'bench-box: the mock box did not start; last log lines:\n' >&2
tail -5 "$BENCH_DIR/logs/box.log" >&2 || true
exit 1