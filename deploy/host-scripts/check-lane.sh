#!/usr/bin/env bash
# check-lane.sh — assert that a lane is what it claims to be.
#
# Runs ON the deploy host. The deploy and the promotion call it before they
# start a service and after they restart one: a lane that points at the other
# lane's database, bucket, port or token is refused before it runs.
#
#   LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_PG_DB LANE_PG_USER
#   LANE_BUCKET LANE_NODE_ID
#   REQUIRE_INSTALLED=1   also assert that a release is installed (after a switch)
#   OTHER_DIR OTHER_*     the other lane, for the isolation checks
set -euo pipefail

fail=0
ok()  { printf '  ok   %s\n' "$*"; }
bad() { printf '  FAIL %s\n' "$*"; fail=1; }

conf="$LANE_DIR/config.toml"
[ -s "$conf" ] || { echo "  FAIL no config at $conf"; exit 1; }
body="$(cat "$conf")"

# --- the lane's identity, as the server reads it --------------------------
has() { case "$body" in *"$1"*) return 0 ;; *) return 1 ;; esac; }

has "$LANE_DIR/repos"        && ok "storage path is $LANE_DIR/repos"       || bad "storage path is not $LANE_DIR/repos"
has "0.0.0.0:$LANE_GITALY_PORT" && ok "server listens on $LANE_GITALY_PORT" || bad "server does not listen on $LANE_GITALY_PORT"
if has "/$LANE_PG_DB?" || has "/$LANE_PG_DB\""; then ok "database is $LANE_PG_DB"; else bad "database is not $LANE_PG_DB"; fi
has "$LANE_PG_USER@"         && ok "database role is $LANE_PG_USER"        || bad "database role is not $LANE_PG_USER"
has "s3://$LANE_BUCKET?"     && ok "bucket is $LANE_BUCKET"                || bad "bucket is not $LANE_BUCKET"
has "node_id = \"$LANE_NODE_ID\"" && ok "node id is $LANE_NODE_ID"         || bad "node id is not $LANE_NODE_ID"

# --- the files that hold secrets are private -----------------------------
for f in "$conf" "$LANE_DIR/tokens/shared" "$LANE_DIR/secrets/gitai.env"; do
  if [ ! -s "$f" ]; then bad "$f is missing"; continue; fi
  mode="$(stat -c '%a' "$f")"
  case "$mode" in
    600|400) ok "$(basename "$f") is $mode" ;;
    *) bad "$(basename "$f") is $mode, expected 600" ;;
  esac
done

# --- the installed release (after a switch) ------------------------------
if [ "${REQUIRE_INSTALLED:-0}" = "1" ]; then
  [ -L "$LANE_DIR/current" ] && ok "current is a symlink ($(basename "$(readlink -f "$LANE_DIR/current")"))" \
    || bad "current is not a symlink"
  [ -L "$LANE_DIR/bin" ] && ok "bin follows current" || bad "bin is not a symlink"
  [ -x "$LANE_DIR/current/bin/gitaly" ] && ok "gitaly is present in the current release" \
    || bad "no gitaly in the current release"
  [ -x "$LANE_DIR/run-gitaly.sh" ] && ok "runner is present" || bad "no runner at $LANE_DIR/run-gitaly.sh"
fi

# --- isolation from the other lane ---------------------------------------
if [ -n "${OTHER_DIR:-}" ] && [ -d "$OTHER_DIR" ]; then
  token_here="$(cat "$LANE_DIR/tokens/shared" 2>/dev/null || true)"
  token_there="$(cat "$OTHER_DIR/tokens/shared" 2>/dev/null || true)"
  if [ -n "$token_here" ] && [ "$token_here" = "$token_there" ]; then
    bad "the two lanes share a token"
  else
    ok "token differs from the other lane"
  fi
  [ "$LANE_PG_DB" != "${OTHER_PG_DB:-}" ] && ok "database differs ($LANE_PG_DB)" \
    || bad "database is shared with the other lane ($LANE_PG_DB)"
  [ "$LANE_BUCKET" != "${OTHER_BUCKET:-}" ] && ok "bucket differs ($LANE_BUCKET)" \
    || bad "bucket is shared with the other lane ($LANE_BUCKET)"
  [ "$LANE_GITALY_PORT" != "${OTHER_GITALY_PORT:-}" ] && ok "server port differs ($LANE_GITALY_PORT)" \
    || bad "server port is shared with the other lane"
  [ "$LANE_CACHE_TCP_PORT" != "${OTHER_CACHE_TCP_PORT:-}" ] && ok "cache port differs ($LANE_CACHE_TCP_PORT)" \
    || bad "cache port is shared with the other lane"
fi

exit "$fail"
