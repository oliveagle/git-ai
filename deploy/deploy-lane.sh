#!/usr/bin/env bash
# deploy-lane.sh — put a release on a lane and make the lane run it.
#
#   deploy/deploy-lane.sh <prod|staging> <bundle.tar.gz>
#
#   DEPLOY_VERSION=<version>          name the release instead of reading it
#                                     from the bundle's file name
#   BUNDLE_ALREADY_ON_HOST=1          the bundle is already at
#                                     <lane>/releases/incoming/ — do not upload
#                                     it. The promotion uses this so that the
#                                     bytes which passed the gate are the bytes
#                                     the production lane installs.
#
# The same script serves both lanes, which is what makes the staging lane a
# rehearsal of the production one: the code path, the layout and the checks are
# identical and only the lane's settings differ.
#
# A deploy is: stage the bundle -> prepare (directory, secrets, PostgreSQL role
# and database, bucket, config) -> check that the lane is what it claims ->
# install the release -> switch the current symlink and restart -> check and
# probe -> roll back to the release that ran before if the probe fails.
set -euo pipefail

# shellcheck source=deploy/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

lane="${1:-}"
bundle="${2:-}"
case "$lane" in
  prod|staging) ;;
  *) die "usage: deploy-lane.sh <prod|staging> <bundle.tar.gz>" ;;
esac
if [ "${BUNDLE_ALREADY_ON_HOST:-0}" != "1" ]; then
  [ -s "$bundle" ] || die "no bundle at $bundle"
fi

if [ -n "${DEPLOY_VERSION:-}" ]; then
  version="$DEPLOY_VERSION"
else
  version="$(basename "$bundle")"
  version="${version%.tar.gz}"
  version="${version#git-ai-release-}"
fi

export LANE="$lane"
export LANE_DIR="$(lane_dir "$lane")"
export LANE_GITALY_PORT="$(lane_gitaly_port "$lane")"
export LANE_CACHE_TCP_PORT="$(lane_cache_port "$lane")"
export LANE_CACHE_METRICS_PORT="$(lane_metrics_port "$lane")"
export LANE_PG_DB="$(lane_pg_db "$lane")"
export LANE_PG_USER="$(lane_pg_user "$lane")"
export LANE_BUCKET="$(lane_bucket "$lane")"
export LANE_NODE_ID="$(lane_node_id "$lane")"
export LANE_CONTAINER="$(lane_container "$lane")"
export LANE_PROGRAMS="$(lane_programs "$lane")"

if [ "$lane" = "staging" ]; then other="prod"; else other="staging"; fi
export OTHER_DIR="$(lane_dir "$other")"
export OTHER_PG_DB="$(lane_pg_db "$other")"
export OTHER_BUCKET="$(lane_bucket "$other")"
export OTHER_GITALY_PORT="$(lane_gitaly_port "$other")"
export OTHER_CACHE_TCP_PORT="$(lane_cache_port "$other")"

export VERSION="$version"
export EXPECTED_VERSION="$version"
export BUNDLE_REMOTE="$LANE_DIR/releases/incoming/git-ai-release-$version.tar.gz"
export MK_BUCKET="$LANE_DIR/.tools/bin/mkbucket"

log "deploying $version to the $lane lane ($LANE_DIR)"

if [ "${BUNDLE_ALREADY_ON_HOST:-0}" = "1" ]; then
  log "using the bundle already on the host"
  ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" \
    "test -s '$BUNDLE_REMOTE'" || die "no bundle at $BUNDLE_REMOTE on the host"
  ssh_script "$DEPLOY_DIR/host-scripts/stage-bundle.sh" LANE_DIR VERSION BUNDLE_REMOTE
else
  log "uploading the bundle"
  ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" "mkdir -p '$LANE_DIR/releases/incoming'"
  ssh_put "$bundle" "$BUNDLE_REMOTE"
  ssh_script "$DEPLOY_DIR/host-scripts/stage-bundle.sh" LANE_DIR VERSION BUNDLE_REMOTE
fi

log "preparing the lane"
ssh_script "$DEPLOY_DIR/host-scripts/prepare-lane.sh" \
  LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_CACHE_METRICS_PORT \
  LANE_PG_DB LANE_PG_USER LANE_BUCKET LANE_NODE_ID LANE_CONTAINER LANE_PROGRAMS \
  DEPLOY_BASE PG_HOST PG_PORT PG_CONTAINER PG_IN_CONTAINER_PORT \
  RUSTFS_ENDPOINT RUSTFS_REGION RUSTFS_CONTAINER MK_BUCKET

log "checking the lane before it starts"
ssh_script "$DEPLOY_DIR/host-scripts/check-lane.sh" \
  LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_PG_DB LANE_PG_USER \
  LANE_BUCKET LANE_NODE_ID OTHER_DIR OTHER_PG_DB OTHER_BUCKET \
  OTHER_GITALY_PORT OTHER_CACHE_TCP_PORT

log "installing the release"
ssh_script "$DEPLOY_DIR/host-scripts/install-release.sh" \
  LANE_DIR LANE_CONTAINER LANE_PROGRAMS VERSION BUNDLE_REMOTE

previous="$(ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" \
  "readlink -f '$LANE_DIR/current' 2>/dev/null | sed 's#.*/##' || true")"

log "switching to $version${previous:+ (from $previous)}"
ssh_script "$DEPLOY_DIR/host-scripts/switch-current.sh" \
  LANE_DIR LANE_CONTAINER LANE_PROGRAMS VERSION

healthy=0
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  sleep 2
  if ssh_script "$DEPLOY_DIR/host-scripts/healthcheck.sh" \
      LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_CACHE_METRICS_PORT \
      LANE_CONTAINER LANE_PG_DB LANE_PG_USER LANE_BUCKET EXPECTED_VERSION \
      MK_BUCKET PG_CONTAINER PG_IN_CONTAINER_PORT RUSTFS_ENDPOINT RUSTFS_REGION \
      >"/tmp/healthcheck.$lane.log" 2>&1; then
    healthy=1
    break
  fi
  log "health check attempt $attempt failed"
done
sed 's/^/  /' "/tmp/healthcheck.$lane.log" >&2 || true

if [ "$healthy" != "1" ]; then
  if [ -n "$previous" ]; then
    warn "the release is not healthy, rolling the lane back to $previous"
    ssh_script "$DEPLOY_DIR/host-scripts/switch-current.sh" \
      LANE_DIR LANE_CONTAINER LANE_PROGRAMS previous
    sleep 3
    ssh_script "$DEPLOY_DIR/host-scripts/healthcheck.sh" \
      LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_CACHE_METRICS_PORT \
      LANE_CONTAINER LANE_PG_DB LANE_PG_USER LANE_BUCKET previous \
      MK_BUCKET PG_CONTAINER PG_IN_CONTAINER_PORT RUSTFS_ENDPOINT RUSTFS_REGION \
      || warn "the lane is not healthy after the rollback either"
  fi
  die "deploy of $version to $lane failed"
fi

ssh_script "$DEPLOY_DIR/host-scripts/check-lane.sh" \
  LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_PG_DB LANE_PG_USER \
  LANE_BUCKET LANE_NODE_ID OTHER_DIR OTHER_PG_DB OTHER_BUCKET \
  OTHER_GITALY_PORT OTHER_CACHE_TCP_PORT REQUIRE_INSTALLED

log "release $version is live on the $lane lane"
