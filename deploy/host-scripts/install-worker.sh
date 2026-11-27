#!/usr/bin/env bash
# install-worker.sh — put the job worker of a node on the node
#
# Runs ON the node that runs the jobs:
#
#   install-worker.sh [--from PATH] [--repo DIR] [--prefix DIR]
#                      [--env FILE] [--unit FILE] [--no-start] [--no-register]
#
# The worker runs the periodic jobs of the node on a Temporal server: the
# sweep of the mirror queue, the tiering of its fallbacks, the backup gate of
# the storage core and the dumps of its database. The schedule lives on the
# server, the work lives here, and the history of a run is something a person
# can read.
#
# The binary is built without cgo, so the same file runs on every node. This
# script installs the binary, writes the systemd user unit, starts the
# service and registers the schedules of the jobs of this node on the
# Temporal server: a worker without a calendar runs nothing.
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
REGISTER=1

while [ $# -gt 0 ]; do
  case "$1" in
    --from)     FROM="${2:?--from needs a path}"; shift 2 ;;
    --repo)     REPO="${2:?--repo needs a directory}"; shift 2 ;;
    --prefix)   PREFIX="${2:?--prefix needs a directory}"; shift 2 ;;
    --env)      ENV_FILE="${2:?--env needs a file}"; shift 2 ;;
    --unit)     UNIT_FILE="${2:?--unit needs a file}"; shift 2 ;;
    --no-start) START=0; shift ;;
    --no-register) REGISTER=0; shift ;;
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

# --- the calendar --------------------------------------------------------
# The schedule of the jobs lives on the Temporal server, and a worker without
# a calendar runs nothing. The install therefore puts the schedules of the
# jobs of this node on the server. The register is a create-or-update, so it
# may run again after a change of the configuration: the runs a schedule had
# keep their history.
#
# The environment of the register is the environment file of the worker, read
# the way systemd reads it: a line that assigns a variable is exported, a
# comment is not, and the quotes around a value are the quotes of the file
# rather than of the value — a cron expression that keeps its quotes is not a
# cron expression. The file holds no secret: it holds the paths of the files
# that hold them.
[ "$REGISTER" = "1" ] || exit 0
log "registering the schedules of the jobs of this node"
while IFS= read -r line; do
  case "$line" in
    ''|'#'*) continue ;;
  esac
  key="${line%%=*}"
  value="${line#*=}"
  case "$value" in
    \"*\") value="${value#\"}"; value="${value%\"}" ;;
    \'*\') value="${value#\'}"; value="${value%\'}" ;;
  esac
  export "$key=$value"
done < "$ENV_FILE"
"$PREFIX/bin/graft-worker" register
# The list is the proof: a job that is on the calendar names the workflow it
# starts and the next minute it runs.
"$PREFIX/bin/graft-worker" list
