#!/usr/bin/env bash
# rollback.sh — point a lane back at a release it already has installed.
#
#   deploy/rollback.sh [prod|staging] [version]
#
# Without a version the newest installed release other than the one the lane
# runs now is used, which is what an operator wants after a promotion went
# wrong. The switch is a symlink swap plus a restart, and the health check
# after it is the same one a deploy runs; nothing is built, downloaded or
# installed here, so a rollback cannot fail for want of a bundle.
set -euo pipefail

# shellcheck source=deploy/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

lane="${1:-prod}"
case "$lane" in
  prod|staging) ;;
  *) die "usage: rollback.sh [prod|staging] [version]" ;;
esac

export LANE="$lane"
export LANE_DIR="$(lane_dir "$lane")"
export LANE_GITALY_PORT="$(lane_gitaly_port "$lane")"
export LANE_CACHE_TCP_PORT="$(lane_cache_port "$lane")"
export LANE_CACHE_METRICS_PORT="$(lane_metrics_port "$lane")"
export LANE_PG_DB="$(lane_pg_db "$lane")"
export LANE_PG_USER="$(lane_pg_user "$lane")"
export LANE_BUCKET="$(lane_bucket "$lane")"
export LANE_CONTAINER="$(lane_container "$lane")"
export LANE_PROGRAMS="$(lane_programs "$lane")"
export MK_BUCKET="$LANE_DIR/.tools/bin/mkbucket"

current="$(lane_current_version "$lane")"
target="${2:-}"
if [ -z "$target" ]; then
  # The newest installed release, other than the one the lane runs now, that
  # actually carries a server binary: releases/incoming holds bundles, not
  # releases, and a half-installed directory has no gitaly.
  listing="$(ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" \
    "ls -1dt '$LANE_DIR'/releases/*/ 2>/dev/null || true")"
  while read -r dir; do
    [ -n "$dir" ] || continue
    candidate="$(basename "$dir")"
    [ "$candidate" = "$current" ] && continue
    if ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" "test -x '$dir/bin/gitaly'"; then
      target="$candidate"
      break
    fi
  done <<<"$listing"
fi
[ -n "$target" ] || die "no other installed release to roll back to in $LANE_DIR/releases"
[ "$target" != "$current" ] || die "$lane already runs $target"

log "rolling the $lane lane back from $current to $target"
export VERSION="$target"
ssh_script "$DEPLOY_DIR/host-scripts/switch-current.sh" \
  LANE_DIR LANE_CONTAINER LANE_PROGRAMS VERSION

healthy=0
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  sleep 2
  if ssh_script "$DEPLOY_DIR/host-scripts/healthcheck.sh" \
      LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_CACHE_METRICS_PORT \
      LANE_CONTAINER LANE_PG_DB LANE_PG_USER LANE_BUCKET VERSION \
      MK_BUCKET PG_CONTAINER PG_IN_CONTAINER_PORT RUSTFS_ENDPOINT RUSTFS_REGION \
      >"/tmp/healthcheck.$lane.log" 2>&1; then
    healthy=1
    break
  fi
  log "health check attempt $attempt failed"
done
sed 's/^/  /' "/tmp/healthcheck.$lane.log" >&2 || true
[ "$healthy" = "1" ] || die "the $lane lane is not healthy on $target; it still runs $target"

log "the $lane lane now runs $target (was $current)"
