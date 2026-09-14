#!/usr/bin/env bash
# Copy a paired Soloist session from a local data dir into the /data of a
# running pod (the directory mounted from the soloist-session-data PVC).
#
# Usage:
#   ./scripts/migrate-session.sh SOURCE_DIR TARGET [--dry-run]
#
#   SOURCE_DIR  local soloist data dir, e.g. container/soloist-data/
#   TARGET      deployment/teddycloud-spotify-shim or a concrete pod name
#   --dry-run   show the changes without touching the target
#
# Copies .device_id, settings/ and the complete auth cache/ (dbrts,
# cache/Users/<id>, public.ldb, Storage ...) in one shot, then deletes the
# pod so the Deployment controller (or ArgoCD) restarts it. Soloist must not
# start against the old state — hence the mandatory restart step — otherwise
# it clobbers the fresh token with an empty one.
#
# bin/, crashpad/, /cache content and runtime state (soloist.pid, ws.addr,
# ws.port, .lock) are deliberately left behind. Stop the LOCAL shim before
# migrating so the source session files are not mid-write while copied.

set -euo pipefail

usage() {
    sed -n '1,25p' "$0"
    exit 2
}

[ $# -ge 2 ] || usage

DRY_RUN=0
for arg in "$@"; do
    [ "$arg" = "--dry-run" ] && DRY_RUN=1
done

SOURCE_DIR="${1%/}"
TARGET="$2"

[ -d "$SOURCE_DIR" ] || { echo "error: SOURCE_DIR is not a directory: $SOURCE_DIR" >&2; exit 1; }
[ -f "$SOURCE_DIR/.device_id" ] || { echo "error: missing $SOURCE_DIR/.device_id (not a soloist data dir?)" >&2; exit 1; }
[ -d "$SOURCE_DIR/settings/Users" ] || { echo "error: missing $SOURCE_DIR/settings/Users" >&2; exit 1; }
if ! { [ -f "$SOURCE_DIR/cache/dbrts" ] && [ -s "$SOURCE_DIR/cache/dbrts" ]; }; then
    echo "error: missing or empty $SOURCE_DIR/cache/dbrts (the refresh token is required for auth)" >&2
    exit 1
fi
[ -f "$SOURCE_DIR/cache/client_token" ] || {
    echo "error: missing $SOURCE_DIR/cache/client_token" >&2; exit 1
}

mapfile -t USER_DIRS < <(find "$SOURCE_DIR/settings/Users" -maxdepth 1 -type d -name '*-user' -printf '%f\n')
if [ "${#USER_DIRS[@]}" -ne 1 ]; then
    echo "error: expected exactly one '<user-id>-user' under settings/Users, found ${#USER_DIRS[@]}: ${USER_DIRS[*]:-none}" >&2
    exit 1
fi
USER_DIR="${USER_DIRS[0]}"
DEVICE_ID="$(cat "$SOURCE_DIR/.device_id")"
FILE_COUNT="$(find "$SOURCE_DIR/settings" -type f | wc -l)"
CACHE_FILE_COUNT="$(find "$SOURCE_DIR/cache" -type f | wc -l)"

resolve_pod() {
    case "$TARGET" in
        deployment/*)
            local name="${TARGET#deployment/}"
            oc get pods -l "app=$name" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
            ;;
        *)
            oc get pod "$TARGET" -o jsonpath='{.metadata.name}' 2>/dev/null
            ;;
    esac
}

POD="$(resolve_pod)" || { echo "error: could not resolve target pod for '$TARGET'" >&2; exit 1; }
[ -n "$POD" ] || { echo "error: no pod running for '$TARGET'" >&2; exit 1; }

target_users=""
if ! target_users="$(oc exec "$POD" -- sh -c 'ls -1d /data/settings/Users/*-user 2>/dev/null || true' 2>/dev/null)"; then
    echo "warn: could not inspect $POD /data/settings/Users; assuming no stale session" >&2
    target_users=""
fi

target_exists_stale() {
    local dir
    for dir in $target_users; do
        if [ "${dir##*/}" = "$USER_DIR" ]; then
            return 0
        fi
    done
    return 1
}

STALE=0
[ -n "$target_users" ] && target_exists_stale && STALE=1

if [ "$DRY_RUN" = 1 ]; then
    echo "Would stream to $POD:"
    echo "  .device_id, settings/ ($FILE_COUNT files), cache/ ($CACHE_FILE_COUNT files incl. dbrts, Users)"
    [ "$STALE" = 1 ] && echo "  (stale session for same user would be overwritten in place)"
    echo "Then would delete pod $POD to trigger a restart with the fresh token"
    exit 0
fi

echo "copying session bundle to $POD ..."
if [ "$STALE" = 1 ]; then
    case "$USER_DIR" in
        *-user) oc exec "$POD" -- rm -rf "/data/settings/Users/$USER_DIR" ;;
        *) echo "error: refusing to delete non user dir: $USER_DIR" >&2; exit 1 ;;
    esac
fi

tar czf - -C "$SOURCE_DIR" .device_id settings cache | oc exec -i "$POD" -- tar xzf - -C /data

echo "restarting pod $POD so soloist picks up the fresh token ..."
oc delete "$TARGET" --wait=false

echo "migrated session:"
echo "  user  : $USER_DIR"
echo "  device: $DEVICE_ID"
echo "  copied: .device_id, settings/ ($FILE_COUNT files), cache/ ($CACHE_FILE_COUNT files)"
echo "  restarted: $TARGET"
echo "  verify: oc logs $TARGET | grep auth_state   (expect logged_in:true)"