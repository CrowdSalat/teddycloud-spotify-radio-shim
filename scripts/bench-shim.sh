#!/usr/bin/env bash
# Start the shim for the local test bench.
#
# Invoked by `make bench-shim`. This is a script rather than an inline recipe
# because an inline `setsid … &` inside a make recipe leaves the recipe shell
# holding the child's inherited stdout, so make never returns — the shim comes
# up fine and the target hangs forever. Redirecting every descriptor and
# detaching properly is what makes `make bench-up` terminate.

set -euo pipefail

: "${BENCH_DIR:=$(pwd)/.bench}"
: "${BENCH_SHIM:=http://localhost:8090}"
: "${BENCH_WEB:=http://localhost:8080}"

die() {
	printf 'bench-shim: %s\n' "$*" >&2
	exit 1
}

port_busy() {
	ss -ltn 2>/dev/null | grep -q "$1"
}

# healthz reports the body even on 503, which curl's -f would swallow. The
# readiness reason is the whole point of the probe: "pulseaudio_not_ready" is a
# legitimate state the shim serves while, and it tells the operator exactly what
# to do about it.
healthz() {
	curl -sS -m 3 "$BENCH_SHIM/healthz" 2>/dev/null || echo unreachable
}

# Already healthy: nothing to do, and restarting would throw away a live pairing.
if curl -fsS -m 2 -o /dev/null "$BENCH_SHIM/healthz" 2>/dev/null; then
	printf 'shim: already listening on %s\n' "$BENCH_SHIM"
	exit 0
fi

# Port held by something that is not answering healthz. Starting anyway would
# leave the shim unable to bind while make reported success, so refuse and say
# what to run.
if port_busy ':8090'; then
	die "port 8090 is busy but not answering healthz — a stale bench?
       run './scripts/cycle.sh down' or 'make bench-stop' first"
fi

mkdir -p "$BENCH_DIR/logs" "$BENCH_DIR/data" "$BENCH_DIR/cache"

# STATIC_STREAM is passed explicitly rather than left to a default: an unset
# variable reaching the shim as empty string is how a bench ends up configured
# for real pairing when the operator asked for a tone (and vice versa).
static_stream="${STATIC_STREAM:-0}"
api_key="${SOLOIST_API_KEY:-test}"
log_level="${LOG_LEVEL:-debug}"

printf 'shim: starting (STATIC_STREAM=%s, log level %s)\n' "$static_stream" "$log_level"

# All three descriptors detached, and stdin closed: Soloist inherits this
# process's environment and a lingering stdin keeps a controlling terminal
# attached that the teardown then has to hunt for.
#
# The redirection is on the *subshell*, not just the command inside it. A
# backgrounded subshell inherits the parent's stdout, and when this script is run
# from a make recipe that stdout is make's pipe — so make blocks until the pipe
# closes, i.e. forever, even though the shim came up correctly. Redirecting the
# subshell itself is what lets make return.
(
	cd "$BENCH_DIR" || exit 1
	exec setsid env \
		SOLOIST_API_KEY="$api_key" \
		TEDDYCLOUD_URL="$BENCH_WEB" \
		LISTEN_ADDR=:8090 \
		LOG_LEVEL="$log_level" \
		STATIC_STREAM="$static_stream" \
		SOLOIST_DATA_DIR="$BENCH_DIR/data" \
		SOLOIST_CACHE_DIR="$BENCH_DIR/cache" \
		./shim
) >"$BENCH_DIR/logs/shim.log" 2>&1 </dev/null &

# Wait for the listener rather than sleeping a fixed interval, so a slow start
# does not turn into a race and a fast one does not add dead time.
for _ in $(seq 1 20); do
	if curl -fsS -m 1 -o /dev/null "$BENCH_SHIM/healthz" 2>/dev/null; then
		printf 'shim: %s\n' "$BENCH_SHIM"
		printf '       healthz: %s\n' "$(healthz)"
		printf '       log: .bench/logs/shim.log\n'
		exit 0
	fi
	if port_busy ':8090'; then
		break
	fi
	sleep 1
done

# The listener is up but unhealthy. That is a legitimate state — the shim serves
# /healthz 503 while PulseAudio is still starting, and keeps serving — so report
# the reason instead of failing the target.
if port_busy ':8090'; then
	printf 'shim: %s (listening, but unhealthy)\n' "$BENCH_SHIM"
	printf '       healthz: %s\n' "$(healthz)"
	printf '       log: .bench/logs/shim.log\n'
	if [ "$static_stream" = "0" ]; then
		printf '       note: real pairing needs PulseAudio or PipeWire-pulse.\n'
		printf '             pulseaudio_not_ready? install it, or use STATIC_STREAM=1.\n'
	fi
	exit 0
fi

printf 'bench-shim: the shim did not start; last log lines:\n' >&2
tail -5 "$BENCH_DIR/logs/shim.log" >&2 || true
exit 1