#!/usr/bin/env bash
# mirror-queue-seed.sh — declare the mirror of every repository of a lane.
#
# Runs ON the node host, as the user the mirror worker runs as:
#
#   mirror-queue-seed.sh [--dry-run]
#
# The table below is the operator's statement of where each repository is
# published: one line per destination, named by the remote it lives on. The
# script turns every line into a target of the mirror queue, and adding a
# target is also what registers it with the server — the mirror policy reads
# the registered targets, so a repository whose mirror was never declared
# counts as having none.
#
# It is idempotent. A target the queue already holds is left alone: its
# schedule, its branches and its history belong to whoever added it, and
# re-adding it would reset the last outcome the policy reads. Publish what it
# declared with `graft mirror queue run`.
#
# A destination whose credential is missing fails when it is published, with
# the name of the variable or the path of the key, not here: the queue stores
# the name of the variable and the path of the key, never their content.
set -euo pipefail

ROOT="${GITAI_ROOT:-$HOME/opt/git-ai}"
QUEUE="${GRAFT_MIRROR_QUEUE:-$ROOT/mirror/mirror-queue.json}"
GITHUB_OWNER="${MIRROR_GITHUB_OWNER:-oliveagle}"
WORKSTATION="${MIRROR_WORKSTATION:-oliveagle@192.168.2.106}"
WORKSTATION_ROOT="${MIRROR_WORKSTATION_ROOT:-/home/oliveagle/git-ai-backup}"
SSH_KEY="${MIRROR_SSH_KEY:-$HOME/.ssh/id_ed25519}"
KNOWN_HOSTS="${MIRROR_KNOWN_HOSTS:-$HOME/.ssh/known_hosts}"

export GRAFT_MIRROR_QUEUE="$QUEUE"
export GRAFT_ADDRESS="${GRAFT_ADDRESS:-unix:$ROOT/run/cache.sock}"
export GRAFT_STORAGE="${GRAFT_STORAGE:-default}"
export GRAFT_TOKEN="${GRAFT_TOKEN:-$(cat "$ROOT/tokens/shared")}"
export GRAFT_USER="${GRAFT_USER:-oliveagle}"

graft="$ROOT/bin/graft"
dry_run=0
[ "${1:-}" = "--dry-run" ] && dry_run=1

say() { printf '  %s\n' "$*"; }

# remote | repository | interval | branches
#
# `github` publishes over SSH to port 443 of ssh.github.com: this network drops
# the connection to github.com:443 often enough that an HTTPS push fails more
# often than it succeeds, while ssh.github.com:443 answers. A network where
# HTTPS works can name the mirror as https://github.com/OWNER/REPO.git instead
# and let GITHUB_TOKEN carry the credential; the queue stores the name of the
# variable, never its value.
#
# `workstation` is the machine that keeps the second copy, reached over SSH.
#
# Both are mirrors in the sense of the policy: a place the repository is
# published to, and a place the policy can see.
targets() {
  cat <<'TABLE'
github      laya-workflow.git    5m   main release/* feat/* feature/* merge/* opt/*
github      ole-utils-ci.git     5m   main release/* feat/* feature/* merge/* opt/*
github      fastapi_mojo.git     5m   *
github      ole-eval.git         5m   *
github      maxllm.git           5m   *
workstation laya-workflow.git    1h   main release/* feat/* feature/* merge/* opt/*
workstation ole-utils-ci.git     1h   main release/* feat/* feature/* merge/* opt/*
workstation fastapi_mojo.git     1h   *
workstation ole-eval.git         1h   *
workstation maxllm.git           1h   *
workstation dot_agents.git       1h   *
workstation assistant-demo.git   1h   *
workstation demo.git             1h   *
workstation smoke2.git           1h   *
workstation imp-test.git         1h   *
TABLE
}

name_of() {
  local remote="$1" repo="$2"
  printf '%s-%s\n' "$remote" "${repo%.git}"
}

url_of() {
  local remote="$1" repo="$2"
  case "$remote" in
    github)      printf 'ssh://git@ssh.github.com:443/%s/%s\n' "$GITHUB_OWNER" "$repo" ;;
    workstation) printf 'ssh://%s%s/%s\n' "$WORKSTATION" "$WORKSTATION_ROOT" "$repo" ;;
    *) return 1 ;;
  esac
}

added=0
kept=0
while read -r remote repo interval branches; do
  [ -n "${remote:-}" ] || continue
  name="$(name_of "$remote" "$repo")"
  url="$(url_of "$remote" "$repo")"

  if "$graft" mirror queue show "$name" >/dev/null 2>&1; then
    say "kept    $name"
    kept=$((kept + 1))
    continue
  fi

  # Both remotes are reached with the key above: github over its SSH port and
  # the workstation directly. --ssh-key and --known-hosts name files that the
  # server reads on every run, so a rotated key needs no new target.
  args=(mirror queue add "$name" --repo "$repo" --url "$url" --interval "$interval"
        --ssh-key "$SSH_KEY" --known-hosts "$KNOWN_HOSTS")
  # The patterns are patterns: a shell that expanded them would add the files
  # of the current directory as branches.
  set -f
  for branch in $branches; do
    args+=(--branch "$branch")
  done
  set +f

  if [ "$dry_run" = "1" ]; then
    say "would add $name -> $url"
    continue
  fi
  if ! "$graft" "${args[@]}" >/dev/null; then
    say "FAILED  $name -> $url"
    exit 1
  fi
  say "added   $name -> $url"
  added=$((added + 1))
done < <(targets | sed '/^[[:space:]]*$/d')

printf '%s declared, %s already there (queue %s)\n' "$added" "$kept" "$QUEUE"
[ "$dry_run" = "1" ] || printf 'publish them with: graft mirror queue run\n'
