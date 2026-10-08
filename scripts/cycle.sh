#!/usr/bin/env bash
# Drive the simulated Toniebox.
#
# `make bench-up` starts teddycloud, the shim and a mock box; this script is how
# you act as that box — place a figurine, lift it, swap in another, and watch
# what the shim does. The mock's control surface is plain HTTP, so every action
# here is a curl you could type by hand; this only saves you the UID and ports.
#
# Usage:
#   ./scripts/cycle.sh place [RUID]   put a figurine on the box
#   ./scripts/cycle.sh lift  [RUID]   take it off again
#   ./scripts/cycle.sh cycle [N]       N place/lift cycles, then the report
#   ./scripts/cycle.sh report          measurements so far, as JSON
#   ./scripts/cycle.sh ear  [big|small]        pinch an ear
#   ./scripts/cycle.sh knock [fwd|back]        knock the box
#   ./scripts/cycle.sh tilt  [fwd|back]        tilt the box
#   ./scripts/cycle.sh charger [on|off]         plug in / unplug
#   ./scripts/cycle.sh stall [ms]        stop reading without disconnecting
#   ./scripts/cycle.sh resume            release a stall
#   ./scripts/cycle.sh drop              drop the RTNL connection
#   ./scripts/cycle.sh log [shim|box|teddycloud]  tail one component
#   ./scripts/cycle.sh down              tear the bench down
#
# Every action prints what it did. The interesting ones are `place` and `lift`:
# those are the signals the shim's whole control path is built from, so they are
# the two to watch when hunting the silence-after-swap defect.

set -euo pipefail

: "${BENCH_ADDR:=http://localhost:9095}"
: "${SHIM_ADDR:=http://localhost:8090}"
: "${TEDDY_WEB:=http://localhost:8080}"
: "${TEDDY_API:=https://localhost:8443}"
: "${BENCH_CONTAINER:=teddycloud-e2e}"
: "${BENCH_RUID:=E00403500EEA4BF2}"

die() {
	printf 'cycle.sh: %s\n' "$*" >&2
	exit 1
}

# need_bench fails early with an actionable message rather than a curl error,
# because the commonest cause is simply that `make bench-up` has not been run.
need_bench() {
	if ! curl -fsS -m 3 "$BENCH_ADDR/" >/dev/null 2>&1; then
		die "no simulated box at $BENCH_ADDR — run 'make bench-up' first"
	fi
}

cmd_place() {
	local uid="${1:-$BENCH_RUID}"
	need_bench
	printf 'placing %s\n' "$uid"

	local out
	out="$(mktemp)"
	if ! curl -fsS -m 180 -X POST "$BENCH_ADDR/control/place?uid=$uid" -o "$out"; then
		rm -f "$out"
		die "place failed — check './scripts/cycle.sh log teddycloud'"
	fi

	jq -r '"  route: \(.Path)  received: \(.TotalReceived)  audio_id: \(.Downloads[0].AudioID // "none")"' "$out" || cat "$out"
	rm -f "$out"
}

cmd_lift() {
	local uid="${1:-$BENCH_RUID}"
	need_bench
	printf 'lifting %s\n' "$uid"
	curl -fsS -m 30 -X POST "$BENCH_ADDR/control/lift?uid=$uid" -o /dev/null -w 'lifted (HTTP %{http_code})\n'
}

# cycle runs the load generator and prints the measurement table. This is the
# reproduction that replaced "5 cycles with an operator and a stopwatch".
cmd_cycle() {
	local count="${1:-5}"
	need_bench
	printf 'running %s place/lift cycles\n' "$count"

	# The report is written to a file first so a jq problem cannot be mistaken for
	# a failed run, and so the raw JSON is always available to inspect.
	local out
	out="$(mktemp)"
	if ! curl -fsS -m 900 -X POST "$BENCH_ADDR/control/cycle?count=$count" -o "$out"; then
		rm -f "$out"
		die "cycle request failed — is the bench up? ('make bench-up')"
	fi

	jq -r '.measurements[]
		| "\(.name)=\(.value)"
		+ (if .pass == false then "  FAIL"
		   elif .pass == true then "  ok"
		   else "" end)' "$out" || {
		printf 'cycle.sh: could not parse the report; raw JSON follows\n' >&2
		cat "$out" >&2
		rm -f "$out"
		die "malformed report"
	}

	if ! jq -e '.measurements | all(.pass != false)' "$out" >/dev/null; then
		printf '\nsome assertions failed; ./scripts/cycle.sh report for the detail\n' >&2
	fi
	rm -f "$out"
}

cmd_report() {
	need_bench
	curl -fsS -m 10 "$BENCH_ADDR/control/report" | jq .
}

cmd_log() {
	case "${1:-shim}" in
	shim)
		tail -f "$BENCH_STATE/shim.log"
		;;
	box)
		tail -f "$BENCH_STATE/box.log"
		;;
	teddycloud)
		podman logs -f --since 2m "$BENCH_CONTAINER" 2>&1
		;;
	*)
		die "unknown log '$1' (want shim, box or teddycloud)"
		;;
	esac
}

cmd_down() {
	: "${BENCH_STATE:=$(pwd)/.bench}"

	# Order matters: the shim spawns Soloist, so stopping the shim first lets it
	# reap the child. Matching on the state dir rather than the bare name is what
	# keeps this from killing an unrelated soloist or a shim the operator is
	# running elsewhere.
	#
	# SIGTERM rather than pkill's default SIGTERM-with-no-wait: Soloist installs
	# crashpad handlers, and a clean signal lets it flush its session files. A
	# truncated session is worse than none, because it looks paired but is not.
	pkill -f "\./shim" 2>/dev/null || true
	pkill -f "$BENCH_STATE/.*/bin/soloist" 2>/dev/null || true
	pkill -f 'mock-toniebox' 2>/dev/null || true

	# Wait for the ports to actually free before reporting success. Without this
	# an immediate `bench-up` after `bench-down` finds the old shim still holding
	# 8090, skips starting a new one, and silently runs against stale config —
	# which is exactly how a STATIC_STREAM setting gets ignored.
	local waited=0
	while ss -ltn 2>/dev/null | grep -qE ':8090|:9095'; do
		if [ "$waited" -ge 10 ]; then
			printf 'cycle.sh: ports still busy after 10s; a previous bench may be running\n' >&2
			ss -ltnp 2>/dev/null | grep -E ':8090|:9095' >&2 || true
			break
		fi
		sleep 1
		waited=$((waited + 1))
	done

	podman stop "$BENCH_CONTAINER" >/dev/null 2>&1 || true

	printf 'bench down\n'
}

cmd_help() {
	sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//; $d'
}

: "${BENCH_STATE:=$(pwd)/.bench}"

main() {
	local action="${1:-help}"
	shift || true

	case "$action" in
	place) cmd_place "$@" ;;
	lift) cmd_lift "$@" ;;
	cycle) cmd_cycle "$@" ;;
	report) cmd_report ;;
	ear | knock | tilt | charger)
		need_bench
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/$action/${1:?need a direction}" \
			-o /dev/null -w "$action ${1} (HTTP %{http_code})\n"
		;;
	stall)
		need_bench
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/stall${1:+?ms=$1}" -o /dev/null -w 'stalling\n'
		;;
	resume)
		need_bench
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/resume" -o /dev/null -w 'resumed\n'
		;;
	drop)
		need_bench
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/disconnect" -o /dev/null -w 'RTNL dropped\n'
		;;
	log) cmd_log "$@" ;;
	down) cmd_down ;;
	help | -h | --help) cmd_help ;;
	*) die "unknown action '$action' (try 'cycle.sh help')" ;;
	esac
}

main "$@"