#!/usr/bin/env bash
# prepare-lane.sh — create or repair one lane of the git-ai deployment.
#
# Runs ON the deploy host. The driver (deploy/deploy-lane.sh) sends it over
# ssh with the lane's settings in the environment:
#
#   LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_CACHE_METRICS_PORT
#   LANE_PG_DB LANE_PG_USER LANE_BUCKET LANE_NODE_ID LANE_CONTAINER LANE_PROGRAMS
#   LANE_SUPERVISOR_INI (the ini that defines the lane's cache tiers)
#   DEPLOY_BASE PG_HOST PG_PORT PG_CONTAINER PG_IN_CONTAINER_PORT
#   RUSTFS_ENDPOINT RUSTFS_REGION RUSTFS_CONTAINER MK_BUCKET
#
# What it writes is idempotent or written only when missing:
#
#   - the token, the PostgreSQL password and the object store credentials of
#     the lane are generated once and never rotated by a deploy (rotating the
#     production token would lock every client out);
#   - the lane's config.toml is written when missing. An existing one is never
#     rewritten: it belongs to the running service. check-lane.sh asserts that
#     it names this lane's database, bucket, port and node.
#   - run-gitaly.sh (the runner the container starts) is always regenerated.
#
# A lane owns: a directory, a PostgreSQL role and database, a bucket, a token,
# two server ports (server and cache), a container, and two supervisor
# programs for the cache tiers.
set -euo pipefail

say() { printf '  %s\n' "$*"; }

random_hex() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# --- directories ----------------------------------------------------------
mkdir -p "$LANE_DIR"/{run,repos,logs,secrets,tokens,releases,reports}
chmod 700 "$LANE_DIR/secrets" "$LANE_DIR/tokens"
say "directory $LANE_DIR ready"

# --- the lane's token -----------------------------------------------------
# Two lanes never share a token: a client of one is refused by the other.
token_file="$LANE_DIR/tokens/shared"
if [ ! -s "$token_file" ]; then
  umask 077
  random_hex 32 >"$token_file"
  chmod 600 "$token_file"
  say "token generated ($token_file)"
else
  say "token kept ($token_file)"
fi

# --- the lane's environment ----------------------------------------------
# gitai.env holds what the server process reads: the PostgreSQL password of
# the lane's role and the object store's service account.
env_file="$LANE_DIR/secrets/gitai.env"
if [ ! -s "$env_file" ]; then
  pg_password="$(random_hex 16)"

  # The object store account: the store has no per-lane account yet, so the
  # lane borrows the account production already uses. Bootstrapping the very
  # first lane falls back to the store's own container environment.
  store_env="$DEPLOY_BASE/secrets/gitai.env"
  if [ -s "$store_env" ]; then
    aws_key="$(sed -n 's/^AWS_ACCESS_KEY_ID=//p' "$store_env")"
    aws_secret="$(sed -n 's/^AWS_SECRET_ACCESS_KEY=//p' "$store_env")"
  else
    store_env_dump="$(docker inspect "$RUSTFS_CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}')"
    aws_key="$(printf '%s\n' "$store_env_dump" | sed -n 's/^RUSTFS_ACCESS_KEY=//p')"
    aws_secret="$(printf '%s\n' "$store_env_dump" | sed -n 's/^RUSTFS_SECRET=//p')"
  fi
  [ -n "$aws_key" ] && [ -n "$aws_secret" ] || { echo "no object store credentials found" >&2; exit 1; }

  umask 077
  cat >"$env_file" <<ENV
PGHOST=$PG_HOST
PGPORT=$PG_PORT
PGUSER=$LANE_PG_USER
PGPASSWORD=$pg_password
PGDATABASE=$LANE_PG_DB
PGSSLMODE=disable
AWS_ACCESS_KEY_ID=$aws_key
AWS_SECRET_ACCESS_KEY=$aws_secret
AWS_REGION=$RUSTFS_REGION
ENV
  chmod 600 "$env_file"
  say "secrets written ($env_file)"
else
  say "secrets kept ($env_file)"
fi

pg_password="$(sed -n 's/^PGPASSWORD=//p' "$env_file")"
[ -n "$pg_password" ] || { echo "no PGPASSWORD in $env_file" >&2; exit 1; }

# --- the PostgreSQL role and database of the lane -------------------------
# The role is created with the lane's own password and may create the lane's
# database. An existing role keeps its password: the lane's server is already
# using it.
psql_super() {
  docker exec -u postgres "$PG_CONTAINER" psql -p "$PG_IN_CONTAINER_PORT" -v ON_ERROR_STOP=1 "$@"
}

role_exists="$(psql_super -d postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname = '$LANE_PG_USER'" | tr -d '[:space:]')"
if [ "$role_exists" != "1" ]; then
  psql_super -d postgres -c "CREATE ROLE \"$LANE_PG_USER\" LOGIN PASSWORD '$pg_password' CREATEDB" >/dev/null
  say "role $LANE_PG_USER created"
fi

db_exists="$(psql_super -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '$LANE_PG_DB'" | tr -d '[:space:]')"
if [ "$db_exists" != "1" ]; then
  psql_super -d postgres -c "CREATE DATABASE \"$LANE_PG_DB\" OWNER \"$LANE_PG_USER\"" >/dev/null
  say "database $LANE_PG_DB created"
fi
say "postgres ready (role $LANE_PG_USER, database $LANE_PG_DB)"

# --- the bucket of the lane ----------------------------------------------
set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
"$MK_BUCKET" -endpoint "$RUSTFS_ENDPOINT" -region "$RUSTFS_REGION" -bucket "$LANE_BUCKET" >/dev/null
say "bucket $LANE_BUCKET ready"

# --- the lane's configuration -------------------------------------------
config="$LANE_DIR/config.toml"
if [ ! -s "$config" ]; then
  token="$(cat "$token_file")"
  umask 077
  cat >"$config" <<CONF
# git-ai (graft) lane "$LANE" — generated by deploy/host-scripts/prepare-lane.sh
bin_dir      = "$LANE_DIR/bin"
socket_path  = "$LANE_DIR/run/gitaly.sock"
listen_addr  = "0.0.0.0:$LANE_GITALY_PORT"
runtime_dir  = "$LANE_DIR/run"

[auth]
token = "$token"

[[storage]]
name = "default"
path = "$LANE_DIR/repos"

[graft]
standalone = true

[graft.storage]
mode = "authoritative"
database = "postgres://$LANE_PG_USER@$PG_HOST:$PG_PORT/$LANE_PG_DB?sslmode=disable"
bucket = "s3://$LANE_BUCKET?region=$RUSTFS_REGION&endpoint=$RUSTFS_ENDPOINT&s3ForcePathStyle=true&request_checksum_calculation=when_required"
node_id = "$LANE_NODE_ID"
gc = "auto"

[graft.object_service]
max_objects = 1000
chunk_size = 1048576

[graft.changelog]
disabled = false

[graft.derived]
disabled = false
eager = false
CONF
  chmod 600 "$config"
  say "config written ($config)"
else
  say "config kept ($config)"
fi

# --- the runner the container starts ------------------------------------
# A stable path outside the releases: it follows the current symlink, so a
# promotion is a symlink switch and nothing else.
runner="$LANE_DIR/run-gitaly.sh"
cat >"$runner" <<RUNNER
#!/bin/sh
# Generated by deploy/host-scripts/prepare-lane.sh — do not edit by hand.
set -a
. "$LANE_DIR/secrets/gitai.env"
set +a
# The token's validity window is widened for clients whose clock drifts
# (auth/token.go reads this variable; the default stays 30 seconds).
export GITALY_TOKEN_VALIDITY=10m
exec "$LANE_DIR/current/bin/gitaly" serve "$LANE_DIR/config.toml"
RUNNER
chmod 750 "$runner"
say "runner ready ($runner)"

# --- the cache tiers ----------------------------------------------------
# The cache reads through the lane's socket and serves the lane's port, so a
# client of one lane cannot reach the other's objects.
ini="$HOME/.supervisord/configs.d/${LANE_SUPERVISOR_INI:-git-ai-$LANE.ini}"
mkdir -p "$(dirname "$ini")"
prog_unix="$(printf '%s\n' "$LANE_PROGRAMS" | awk '{print $1}')"
prog_tcp="$(printf '%s\n' "$LANE_PROGRAMS" | awk '{print $2}')"
cat >"$ini" <<INI
; Generated by deploy/host-scripts/prepare-lane.sh — the "$LANE" lane's cache.
[program:$prog_unix]
command=$LANE_DIR/bin/graft-cache --listen unix:$LANE_DIR/run/cache.sock --upstream unix:$LANE_DIR/run/gitaly.sock --tier daemon --max-bytes 1073741824 --metrics-addr 127.0.0.1:$((LANE_CACHE_METRICS_PORT - 1))
user=$USER
numprocs=1
priority=20
autostart=true
startsecs=2
startretries=3
autorestart=true
stopsignal=INT
stdout_logfile=$LANE_DIR/logs/cache.log
stderr_logfile=$LANE_DIR/logs/cache.err

[program:$prog_tcp]
command=$LANE_DIR/bin/graft-cache --listen tcp://0.0.0.0:$LANE_CACHE_TCP_PORT --upstream unix:$LANE_DIR/run/gitaly.sock --tier daemon --max-bytes 1073741824 --metrics-addr 127.0.0.1:$LANE_CACHE_METRICS_PORT
user=$USER
numprocs=1
priority=21
autostart=true
startsecs=2
startretries=3
autorestart=true
stopsignal=INT
stdout_logfile=$LANE_DIR/logs/cache-tcp.log
stderr_logfile=$LANE_DIR/logs/cache-tcp.err
INI
say "supervisor programs written ($ini)"

echo "lane $LANE prepared"
