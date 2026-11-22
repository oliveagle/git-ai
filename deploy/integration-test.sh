#!/usr/bin/env bash
# integration-test.sh — run the gate against a lane.
#
#   deploy/integration-test.sh <staging|prod> [version]
#
# The suite runs on the deploy host, where the lane's token, its database and
# the object store are reachable, and writes a JSON report there. Its exit
# code is the gate: 0 means every case passed.
#
# The staging lane is the one the gate is meant for; running it against
# production is possible but writes test repositories into production data.
set -euo pipefail

# shellcheck source=deploy/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

lane="${1:-staging}"
case "$lane" in
  prod|staging) ;;
  *) die "usage: integration-test.sh <prod|staging> [version]" ;;
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
export VERSION="${2:-$(lane_current_version "$lane")}"

if [ "$lane" = "staging" ]; then other="prod"; else other="staging"; fi
export OTHER_DIR="$(lane_dir "$other")"
export OTHER_PG_DB="$(lane_pg_db "$other")"
export OTHER_BUCKET="$(lane_bucket "$other")"

[ -n "$VERSION" ] || die "no version given and the lane reports none"

log "running the integration suite against the $lane lane (release $VERSION)"
set +e
output="$(ssh_script "$DEPLOY_DIR/host-scripts/integration-suite.sh" \
  LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_CACHE_METRICS_PORT \
  LANE_PG_DB LANE_PG_USER LANE_BUCKET VERSION LANE_CONTAINER \
  PG_CONTAINER PG_IN_CONTAINER_PORT RUSTFS_ENDPOINT RUSTFS_REGION \
  OTHER_DIR OTHER_PG_DB OTHER_BUCKET 2>&1)"
rc=$?
set -e
printf '%s\n' "$output"

report_path="$(printf '%s\n' "$output" | sed -n 's/^report: //p' | tail -1)"
if [ -n "$report_path" ]; then
  log "report: $report_path"
  ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" "cat '$report_path'" 2>/dev/null || true
else
  warn "the suite printed no report path"
fi

if [ "$rc" -ne 0 ]; then
  die "the integration suite failed on the $lane lane"
fi
log "the integration suite passed on the $lane lane"
