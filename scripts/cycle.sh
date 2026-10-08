#!/usr/bin/env bash
# Drive the simulated Toniebox.
#
# Every action is a POST to the mock's control surface, which this script wraps
# so you do not have to remember the UID and the port. The same thing works
# straight from a shell, and from inside the box container:
#
#   curl -X POST localhost:9095/control/place
#   podman exec mock-toniebox curl -X POST localhost:9095/control/place
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
#   ./scripts/cycle.sh volume [level] [db]     report a speaker level
#   ./scripts/cycle.sh stall [ms]        stop reading without disconnecting
#   ./scripts/cycle.sh resume            release a stall
#   ./scripts/cycle.sh drop              drop the RTNL connection
#   ./scripts/cycle.sh log [shim|box|teddycloud]  tail one component
#
# `place` and `lift` are the two that matter. They produce exactly the signals the
# shim's control path is built from: a placement produces TagValid, and a lift
# produces playback=stopped. A cycle here drives the same code a real figurine
# swap does, which is what makes the silence-after-swap defect reproducible
# without the box.

set -euo pipefail

: "${BENCH_ADDR:=http://localhost:9095}"
: "${BENCH_RUID:=E00403500EEA4BF2}"

die() {
	printf 'cycle.sh: %s\n' "$*" >&2
	exit 1
}

# need_box fails early with something actionable, because the commonest cause is
# simply that the bench is not up.
need_box() {
	if ! curl -fsS -m 3 "$BENCH_ADDR/" >/dev/null 2>&1; then
		die "no simulated box at $BENCH_ADDR — is the bench up? (make bench-up)"
	fi
}

cmd_place() {
	local uid="${1:-$BENCH_RUID}"
	need_box
	printf 'placing %s\n' "$uid"

	# The report goes to a file first, so a jq problem cannot be mistaken for a
	# failed placement and the raw JSON is always available to inspect.
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
	need_box
	printf 'lifting %s\n' "$uid"
	curl -fsS -m 30 -X POST "$BENCH_ADDR/control/lift?uid=$uid" -o /dev/null -w 'lifted (HTTP %{http_code})\n'
}

# cycle runs the load generator and prints the measurement table: the
# reproduction that replaced "5 cycles with an operator and a stopwatch".
cmd_cycle() {
	local count="${1:-5}"
	need_box
	printf 'running %s place/lift cycles\n' "$count"

	local out
	out="$(mktemp)"
	if ! curl -fsS -m 900 -X POST "$BENCH_ADDR/control/cycle?count=$count" -o "$out"; then
		rm -f "$out"
		die "cycle request failed — is the bench up?"
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
	need_box
	curl -fsS -m 10 "$BENCH_ADDR/control/report" | jq .
}

cmd_log() {
	case "${1:-shim}" in
	shim)
		podman logs -f --since 2m shim 2>&1
		;;
	box)
		podman logs -f --since 2m mock-toniebox 2>&1
		;;
	teddycloud)
		podman logs -f --since 2m teddycloud-bench 2>&1
		;;
	*)
		die "unknown log '$1' (want shim, box or teddycloud)"
		;;
	esac
}

cmd_help() {
	sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//; $d'
}

main() {
	local action="${1:-help}"
	shift || true

	case "$action" in
	place) cmd_place "$@" ;;
	lift) cmd_lift "$@" ;;
	cycle) cmd_cycle "$@" ;;
	report) cmd_report ;;
	ear | knock | tilt | charger)
		need_box
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/$action/${1:?need a direction}" \
			-o /dev/null -w "$action ${1} (HTTP %{http_code})\n"
		;;
	volume)
		need_box
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/volume?level=${1:-12}&db=${2:--3}" \
			-o /dev/null -w 'volume reported\n'
		;;
	stall)
		need_box
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/stall${1:+?ms=$1}" -o /dev/null -w 'stalling\n'
		;;
	resume)
		need_box
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/resume" -o /dev/null -w 'resumed\n'
		;;
	drop)
		need_box
		curl -fsS -m 15 -X POST "$BENCH_ADDR/control/disconnect" -o /dev/null -w 'RTNL dropped\n'
		;;
	log) cmd_log "$@" ;;
	help | -h | --help) cmd_help ;;
	*) die "unknown action '$action' (try 'cycle.sh help')" ;;
	esac
}

main "$@"