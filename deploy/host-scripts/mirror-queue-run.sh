#!/usr/bin/env bash
# mirror-queue-run.sh — publish the mirror targets that are due.
#
# Runs ON the node host, from cron or by hand:
#
#   mirror-queue-run.sh                    the targets whose interval has passed
#   mirror-queue-run.sh --all              every enabled target, due or not
#   mirror-queue-run.sh --target NAME      that target, whatever its schedule
#
# This is the worker of the mirror policy. The server records what every
# target last published and decides the next push on that record, so a worker
# that has stopped is a policy that refuses work: it belongs in cron, with a
# log, and not only in a person's shell.
#
# The queue file is the operator's, holds no secret, and names the credential
# of each target by variable or by path; the values are read here, when a task
# runs, and never travel into the queue.
set -euo pipefail

ROOT="${GITAI_ROOT:-$HOME/opt/git-ai}"
export GRAFT_MIRROR_QUEUE="${GRAFT_MIRROR_QUEUE:-$ROOT/mirror/mirror-queue.json}"
export GRAFT_ADDRESS="${GRAFT_ADDRESS:-unix:$ROOT/run/cache.sock}"
export GRAFT_STORAGE="${GRAFT_STORAGE:-default}"
export GRAFT_TOKEN="${GRAFT_TOKEN:-$(cat "$ROOT/tokens/shared")}"
export GRAFT_USER="${GRAFT_USER:-$(id -un)}"

# A target whose mirror is reached over HTTPS names its token variable; the
# value comes from the environment of this process. The GitHub token of this
# host lives in the laya-workflow configuration, which the legacy per-repo
# scripts read too.
token_file="${GITAI_GITHUB_TOKEN_FILE:-$HOME/.config/laya-workflow/github.token}"
if [ -z "${GITHUB_TOKEN:-}" ] && [ -r "$token_file" ]; then
  GITHUB_TOKEN="$(cat "$token_file")"
  export GITHUB_TOKEN
fi

exec "$ROOT/bin/graft" mirror queue run "$@"
