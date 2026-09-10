#!/usr/bin/env bash
# Copy a paired Soloist session from a local data dir into a pod's /data
# (the directory mounted from the soloist-session-data PVC).
#
# Usage:
#   ./scripts/migrate-session.sh SOURCE_DIR TARGET_POD [--dry-run]
#
#   SOURCE_DIR  local soloist data dir, e.g. container/soloist-data/
#   TARGET_POD  target pod whose /data receives the session
#   --dry-run   show the changes without touching the target
#
# Copies only .device_id and settings/ (Users/<user-id>-user plus prefs).
# bin/, cache/, crashpad/ and runtime state (soloist.pid, ws.port, .lock)
# are left behind. If the target already holds a session for the same
# Spotify user, that stale session is deleted first.
#
# Stop the local shim before migrating — the session files must not be
# mid-write while copied.

set -euo pipefail

usage() {
    sed -n '1,20p' "$0"
    exit 2
}

[ $# -ge 2 ] || usage

DRY_RUN=0
for arg in "$@"; do
    [ "$arg" = "--dry-run" ] && DRY_RUN=1
done

SOURCE_DIR="${1%/}"
TARGET_POD="$2"

[ -d "$SOURCE_DIR" ] || { echo "error: SOURCE_DIR is not a directory: $SOURCE_DIR" >&2; exit 1; }
[ -f "$SOURCE_DIR/.device_id" ] || { echo "error: missing $SOURCE_DIR/.device_id (not a soloist data dir?)" >&2; exit 1; }
[ -d "$SOURCE_DIR/settings/Users" ] || { echo "error: missing $SOURCE_DIR/settings/Users" >&2; exit 1; }

mapfile -t USER_DIRS < <(find "$SOURCE_DIR/settings/Users" -maxdepth 1 -type d -name '*-user' -printf '%f\n')
if [ "${#USER_DIRS[@]}" -ne 1 ]; then
    echo "error: expected exactly one '<user-id>-user' under settings/Users, found ${#USER_DIRS[@]}: ${USER_DIRS[*]:-none}" >&2
    exit 1
fi
USER_DIR="${USER_DIRS[0]}"
DEVICE_ID="$(cat "$SOURCE_DIR/.device_id")"
FILE_COUNT="$(find "$SOURCE_DIR/settings" -type f | wc -l)"

target_exists_stale() {
    local dir
    for dir in $target_users; do
        if [ "${dir##*/}" = "$USER_DIR" ]; then
            return 0
        fi
    done
    return 1
}

target_users=""
if ! target_users="$(oc exec "$TARGET_POD" -- sh -c 'ls -1d /data/settings/Users/*-user 2>/dev/null || true' 2>/dev/null)"; then
    echo "warn: could not inspect $TARGET_POD /data/settings/Users; assuming no stale session (pod stopped?)" >&2
    target_users=""
fi

STALE=0
[ -n "$target_users" ] && target_exists_stale && STALE=1

if [ "$DRY_RUN" = 1 ]; then
    if [ "$STALE" = 1 ]; then
        echo "Would delete old session for user $USER_DIR on $TARGET_POD"
    fi
    echo "Would copy settings/Users/$USER_DIR/ ($FILE_COUNT files)"
    echo "Would copy .device_id, settings/prefs"
    exit 0
fi

if [ "$STALE" = 1 ]; then
    case "$USER_DIR" in
        *-user) oc exec "$TARGET_POD" -- rm -rf "/data/settings/Users/$USER_DIR" ;;
        *) echo "error: refusing to delete non user dir: $USER_DIR" >&2; exit 1 ;;
    esac
fi

tar czf - -C "$SOURCE_DIR" .device_id settings | oc exec -i "$TARGET_POD" -- tar xzf - -C /data

echo "migrated session to $TARGET_POD:"
echo "  user  : $USER_DIR"
echo "  device: $DEVICE_ID"
echo "  copied: .device_id, settings/ ($FILE_COUNT files)"
[ "$STALE" = 1 ] && echo "  notes : replaced stale session for the same user"