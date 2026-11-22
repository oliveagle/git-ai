#!/usr/bin/env bash
# healthcheck.sh — is this lane alive, and is it the release it should be?
#
# Runs ON the deploy host:
#
#   LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_CACHE_METRICS_PORT
#   LANE_CONTAINER LANE_PG_DB LANE_PG_USER LANE_BUCKET
#   EXPECTED_VERSION (optional) MK_BUCKET PG_CONTAINER PG_IN_CONTAINER_PORT
#   RUSTFS_ENDPOINT RUSTFS_REGION
#
# The checks are read-only except when the lane holds no repository yet: the
# first one creates a throwaway repository to prove the storage core answers.
set -euo pipefail

fail=0
ok()  { printf '  ok   %s\n' "$*"; }
bad() { printf '  FAIL %s\n' "$*"; fail=1; }

# --- the process ---------------------------------------------------------
running="$(docker inspect -f '{{.State.Running}}' "$LANE_CONTAINER" 2>/dev/null || echo missing)"
if [ "$running" = "true" ]; then ok "container $LANE_CONTAINER is running"
else bad "container $LANE_CONTAINER is not running ($running)"; fi

if [ -S "$LANE_DIR/run/gitaly.sock" ]; then ok "server socket exists"
else bad "no server socket at $LANE_DIR/run/gitaly.sock"; fi

listen_ok() {
  ss -lnt 2>/dev/null | awk -v p=":$1" '$4 ~ p "$" {found=1} END {exit !found}'
}
listen_ok "$LANE_GITALY_PORT" && ok "server listens on $LANE_GITALY_PORT" \
  || bad "nothing listens on $LANE_GITALY_PORT"
listen_ok "$LANE_CACHE_TCP_PORT" && ok "cache listens on $LANE_CACHE_TCP_PORT" \
  || bad "nothing listens on $LANE_CACHE_TCP_PORT"

stats="$(curl -s --max-time 3 "http://127.0.0.1:$LANE_CACHE_METRICS_PORT/stats" 2>/dev/null || true)"
case "$stats" in
  *'"tier"'*) ok "cache stats answer on $LANE_CACHE_METRICS_PORT" ;;
  *) bad "cache stats do not answer on $LANE_CACHE_METRICS_PORT" ;;
esac

# --- the release that runs ----------------------------------------------
current="$(basename "$(readlink -f "$LANE_DIR/current" 2>/dev/null || echo none)")"
if [ -n "${EXPECTED_VERSION:-}" ]; then
  [ "$current" = "$EXPECTED_VERSION" ] && ok "current release is $EXPECTED_VERSION" \
    || bad "current release is $current, expected $EXPECTED_VERSION"
else
  ok "current release is $current"
fi

disk_version="$("$LANE_DIR/current/bin/gitaly" --version 2>/dev/null | sed -n 's/.*version \([^ ]*\).*/\1/p')"
running_version="$(docker logs --tail 500 "$LANE_CONTAINER" 2>&1 \
  | sed -n 's/.*msg="Starting Gitaly".*version=\([^ ]*\).*/\1/p' | tail -1)"
if [ -n "$disk_version" ] && [ "$disk_version" = "$running_version" ]; then
  ok "the process runs the release on disk (gitaly $running_version)"
else
  bad "process version '$running_version' is not the release's '$disk_version'"
fi

# --- the storage core ----------------------------------------------------
token="$(cat "$LANE_DIR/tokens/shared")"
graft="$LANE_DIR/current/bin/graft"
export GRAFT_ADDRESS="tcp://127.0.0.1:$LANE_CACHE_TCP_PORT"
export GRAFT_STORAGE=default
export GRAFT_TOKEN="$token"
export GRAFT_USER="healthcheck"

repo="$(ls -d "$LANE_DIR"/repos/*.git 2>/dev/null | head -1 || true)"
if [ -z "$repo" ]; then
  "$graft" repo create health.git --initial-commit --branch main >/dev/null 2>&1 || true
  repo="$LANE_DIR/repos/health.git"
fi
repo_name="$(basename "$repo")"
if "$graft" storage state -repo "$repo_name" --json >/dev/null 2>&1; then
  ok "storage core answers for $repo_name"
else
  bad "storage core does not answer for $repo_name"
fi

rows="$(docker exec -u postgres "$PG_CONTAINER" psql -p "$PG_IN_CONTAINER_PORT" -d "$LANE_PG_DB" -tAc \
  "SELECT count(*) FROM graft_repositories" 2>/dev/null | tr -d '[:space:]' || true)"
case "$rows" in
  ''|*[!0-9]*) bad "database $LANE_PG_DB does not answer" ;;
  *) ok "database $LANE_PG_DB holds $rows repositories" ;;
esac

set -a
# shellcheck disable=SC1090
. "$LANE_DIR/secrets/gitai.env"
set +a
bucket_out="$("$MK_BUCKET" -endpoint "$RUSTFS_ENDPOINT" -region "$RUSTFS_REGION" -bucket "$LANE_BUCKET" 2>/dev/null || true)"
case "$bucket_out" in
  *exists*) ok "bucket $LANE_BUCKET answers" ;;
  *) bad "bucket $LANE_BUCKET does not answer" ;;
esac

[ "$fail" -eq 0 ] && echo "lane $LANE is healthy" || echo "lane $LANE is NOT healthy"
exit "$fail"
