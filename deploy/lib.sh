#!/usr/bin/env bash
# Shared helpers of the git-ai release pipeline.
#
# Every script in this directory sources this file. It sets the shell
# options, loads the lane definitions and offers the few primitives the
# scripts need: logging, the ssh wrapper to the deploy host, and the
# resolution of a lane's settings by name.

set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/lanes.env
source "${DEPLOY_DIR}/lanes.env"

log()  { printf '[%s] %s\n' "$(date '+%H:%M:%S')" "$*" >&2; }
warn() { printf '[%s] WARN: %s\n' "$(date '+%H:%M:%S')" "$*" >&2; }
die()  { printf '[%s] ERROR: %s\n' "$(date '+%H:%M:%S')" "$*" >&2; exit 1; }

# ssh_run <command...>  — run a shell command on the deploy host.
ssh_run() {
  ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" "bash -s" <<<"$*"
}

# ssh_script <script>  — run a shell script on the deploy host, with the
# lane definitions in the environment. The script is sent over stdin, so it
# can use "$LANE", "$LANE_DIR" and the rest.
ssh_script() {
  local script="$1"
  shift
  {
    printf 'set -euo pipefail\n'
    printf 'DEPLOY_HOME=%q\n'   "$DEPLOY_HOME"
    printf 'DEPLOY_BASE=%q\n'   "$DEPLOY_BASE"
    printf 'PG_HOST=%q\n'       "$PG_HOST"
    printf 'PG_PORT=%q\n'       "$PG_PORT"
    printf 'PG_SUPERUSER=%q\n'  "$PG_SUPERUSER"
    printf 'PG_CONTAINER=%q\n'  "$PG_CONTAINER"
    printf 'PG_IN_CONTAINER_PORT=%q\n' "$PG_IN_CONTAINER_PORT"
    printf 'RUSTFS_ENDPOINT=%q\n' "$RUSTFS_ENDPOINT"
    printf 'RUSTFS_REGION=%q\n' "$RUSTFS_REGION"
    printf 'RUSTFS_CONTAINER=%q\n' "$RUSTFS_CONTAINER"
    for var in "$@"; do
      printf '%s=%q\n' "$var" "${!var:-}"
    done
    cat "$script"
  } | ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" "bash -s"
}

# ssh_put <local path> <remote path>
ssh_put() {
  scp ${DEPLOY_SSH_OPTS} "$1" "${DEPLOY_SSH_USER}@${DEPLOY_HOST}:$2"
}

# lane_setting <lane> <suffix> — resolve STAGING_<suffix> or PROD_<suffix>.
lane_setting() {
  local lane="$1" suffix="$2" name
  case "$lane" in
    staging) name="STAGING_${suffix}" ;;
    prod)    name="PROD_${suffix}" ;;
    *) die "unknown lane: $lane" ;;
  esac
  printf '%s' "${!name}"
}

lane_dir()        { lane_setting "$1" DIR; }
lane_gitaly_port(){ lane_setting "$1" GITALY_PORT; }
lane_cache_port() { lane_setting "$1" CACHE_TCP_PORT; }
lane_metrics_port(){ lane_setting "$1" CACHE_METRICS_PORT; }
lane_pg_db()      { lane_setting "$1" PG_DB; }
lane_pg_user()    { lane_setting "$1" PG_USER; }
lane_bucket()     { lane_setting "$1" BUCKET; }
lane_node_id()    { lane_setting "$1" NODE_ID; }
lane_container()  { lane_setting "$1" CONTAINER; }
lane_programs()   { lane_setting "$1" SUPERVISOR_PROGRAMS; }

# A version string that names a release: a tag, or the commit it was cut from.
release_version() {
  local repo="${1:-.}" explicit="${2:-}"
  if [ -n "$explicit" ]; then
    printf '%s' "$explicit"
    return
  fi
  local described
  described="$(git -C "$repo" describe --tags --match 'v*' --always --dirty 2>/dev/null || true)"
  [ -n "$described" ] || described="$(git -C "$repo" rev-parse --short HEAD)"
  printf '%s' "$described"
}

# The version a lane currently runs, read from the release it points at.
lane_current_version() {
  local lane="$1" dir
  dir="$(lane_dir "$lane")"
  ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" \
    "readlink -f '${dir}/current' 2>/dev/null | sed 's#.*/##' || true"
}
