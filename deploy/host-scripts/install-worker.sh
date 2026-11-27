#!/usr/bin/env bash
# install-worker.sh — put the job worker of a node on the node
#
# Runs ON the node that runs the jobs:
#
#   install-worker.sh [--from PATH] [--repo DIR] [--prefix DIR]
#                      [--env FILE] [--unit FILE] [--no-start]
#
# The worker runs the periodic jobs of the node on a Temporal server: the
# sweep of the mirror queue, the tiering of its fallbacks, the backup gate of
# the storage core and the dumps of its database. The schedule lives on the
# server, the work lives here, and the history of a run is something a person
# can read.
#
# The binary is built without cgo, so the same file runs on every node. This
# script installs the binary, writes the systemd user unit and starts the
# service.
#
# The environment file holds the address of the Temporal server, the files of
# the node and the credential of the graft server. This script never writes
# one: a credential must not pass through a command line, and that file must
# stay at 0600.
set -euo pipefail

PREFIX="${HOME}/opt/git-ai"
ENV_FILE="${HOME}/.config/graft-worker/worker.env"
UNIT_FILE="${HOME}/.config/systemd/user/graft-worker.service"
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
  [ -d "$repo/cmd/graft-worker" ] || die "no cmd/graft-worker under $repo"
  command -v go >/dev/null 2>&1 || die "go is needed to build the worker"
  binary="$PREFIX/bin/.graft-worker.new"
  mkdir -p "$PREFIX/bin"
  log "building the worker from $repo"
  ( cd "$repo" && CGO_ENABLED=0 go build -trimpath -o "$binary" ./cmd/graft-worker )
fi

mkdir -p "$PREFIX/bin"
install -m 0755 "$binary" "$PREFIX/bin/.graft-worker.new"
mv -f "$PREFIX/bin/.graft-worker.new" "$PREFIX/bin/graft-worker"
log "installed $PREFIX/bin/graft-worker"

# --- the configuration ---------------------------------------------------
[ -f "$ENV_FILE" ] || die "no environment file at $ENV_FILE: it holds the configuration and the credentials, and this script never writes one"

# --- the unit ------------------------------------------------------------
mkdir -p "$(dirname "$UNIT_FILE")"
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=graft-worker · the jobs of the node
After=network.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$PREFIX/bin/graft-worker worker
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

# --- the proof that it polls --------------------------------------------
sleep 2
if ! systemctl --user is-active --quiet "$unit"; then
  systemctl --user status "$unit" --no-pager -n 20 >&2 || true
  die "the worker is not running"
fi
log "the worker runs"
