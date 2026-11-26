#!/usr/bin/env bash
# install-observer.sh — put the read model of the storage core on a node
#
# Runs ON the node that serves the read model:
#
#   install-observer.sh [--from PATH] [--repo DIR] [--prefix DIR]
#                       [--env FILE] [--unit FILE] [--no-start]
#
# The observer is the read side of the storage core: it serves the snapshot of
# the repositories, the mirror, the backup and the CI telemetry, and it hosts
# the built frontend. It reads the storage core and never writes to it, so it
# may be stopped and started at any time.
#
# The binary is built without cgo, so the same file runs on every node. This
# script installs the binary, writes the systemd user unit and starts the
# service.
#
# The environment file holds the credentials of the storage core and of the
# naming model. This script never writes one: a password must not pass
# through a command line, and that file must stay at 0600. The listen address
# is read from it, because it is part of the configuration of the service.
set -euo pipefail

PREFIX="${HOME}/opt/git-ai"
ENV_FILE="${HOME}/.config/graft-observer/observer.env"
UNIT_FILE="${HOME}/.config/systemd/user/graft-observer.service"
DEFAULT_LISTEN="127.0.0.1:8789"
FROM=""
REPO=""
START=1

while [ $# -gt 0 ]; do
  case "$1" in
    --from)     FROM="${2:?--from needs a path}"; shift 2 ;;
    --repo)     REPO="${2:?--repo needs a directory}"; shift 2 ;;
    --prefix)   PREFIX="${2:?--prefix needs a directory}"; shift 2 ;;
    --env)      ENV_FILE="${2:?--env needs a file}"; shift 2 ;;
    --unit)     UNIT_FILE="${2:?--unit needs a file}"; shift 2 ;;
    --no-start) START=0; shift ;;
    -h|--help)  sed -n '2,22p' "$0"; exit 0 ;;
    *)          printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

log() { printf '%s %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"; }
die() { printf '%s\n' "$*" >&2; exit 1; }

# --- the binary ----------------------------------------------------------
script_dir="$(cd "$(dirname "$0")" && pwd)"
if [ -n "$FROM" ]; then
  [ -x "$FROM" ] || die "no executable at $FROM"
  binary="$FROM"
else
  repo="${REPO:-$(cd "$script_dir/../.." && pwd)}"
  [ -d "$repo/cmd/graft-observer" ] || die "no cmd/graft-observer under $repo"
  command -v go >/dev/null 2>&1 || die "go is needed to build the observer"
  binary="$PREFIX/bin/.graft-observer.new"
  mkdir -p "$PREFIX/bin"
  log "building the observer from $repo"
  ( cd "$repo" && CGO_ENABLED=0 go build -trimpath -o "$binary" ./cmd/graft-observer )
fi

mkdir -p "$PREFIX/bin"
install -m 0755 "$binary" "$PREFIX/bin/.graft-observer.new"
mv -f "$PREFIX/bin/.graft-observer.new" "$PREFIX/bin/graft-observer"
log "installed $PREFIX/bin/graft-observer"

# --- the configuration ---------------------------------------------------
[ -f "$ENV_FILE" ] || die "no environment file at $ENV_FILE: it holds the credentials, and this script never writes one"
listen="$DEFAULT_LISTEN"
found="$(sed -n 's/^[[:space:]]*GRAFT_OBSERVER_LISTEN[[:space:]]*=[[:space:]]*//p' "$ENV_FILE" | tail -1)"
[ -n "$found" ] && listen="$found"

# --- the unit ------------------------------------------------------------
mkdir -p "$(dirname "$UNIT_FILE")"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=graft-observer · the read model of the storage core
After=network.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$PREFIX/bin/graft-observer
Restart=on-failure
RestartSec=5
WorkingDirectory=$HOME

[Install]
WantedBy=default.target
EOF
log "wrote $UNIT_FILE"

[ "$START" = "1" ] || exit 0

# --- the service ---------------------------------------------------------
command -v systemctl >/dev/null 2>&1 || die "systemctl is needed"
unit="$(basename "$UNIT_FILE")"
loginctl enable-linger "$(id -un)" >/dev/null 2>&1 || log "linger is left as it is"
systemctl --user daemon-reload
systemctl --user enable "$unit"
# A restart, not a start: a new binary must take effect, and a restart also
# starts a service that was not running.
systemctl --user restart "$unit"

# --- the proof that it answers ------------------------------------------
if ! command -v curl >/dev/null 2>&1; then
  log "curl is missing, the service is started but not probed"
  exit 0
fi
for _ in $(seq 1 30); do
  if curl -fsS --max-time 3 "http://$listen/api/health" >/dev/null 2>&1; then
    log "the observer answers on $listen"
    exit 0
  fi
  sleep 1
done
systemctl --user status "$unit" --no-pager -n 20 >&2 || true
die "the observer does not answer on $listen"
