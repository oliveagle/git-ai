#!/usr/bin/env bash
# import-remote.sh — bring the state of a remote repository into the storage core
#
#   import-remote.sh <repo.git> [remote-url] [default-branch]
#
# Runs ON the node that holds the storage core.
#
# Why this exists: the mirror queue publishes the storage core to a remote, and
# only that. A remote that is worked on moves ahead of the storage core, and
# the queue then reports the target as "diverged": it does not push commits it
# does not have, and the commits of the remote stay in the remote. This job
# brings the commits of the remote into the storage core, so that the mirror
# has something to publish and the storage core holds what the remote holds.
#
# How: the objects come from a staging clone of the remote and the reference
# moves through the server. The order matters.
#
#   1. The objects are fetched into the repository of the server *without*
#      moving a reference: the fetch writes FETCH_HEAD, and refs/heads/* is
#      left alone. A reference that is moved behind the server's back leaves
#      the database and the repository of the server disagreeing — and then
#      the server believes the branch is already where the remote put it, and
#      refuses to move it.
#   2. The reference moves through a land, which is the only way the storage
#      core moves one: the server writes the reference, and commits the
#      changeset to the database and to the object store.
#
# `graft storage import` does not move the references of a repository that is
# imported already — it brings in the changelog and the reviews — so it is not
# the way to bring a remote in.
#
# The references of the storage core are read from its database, because the
# database holds the authoritative references. The credential of the remote is
# read from the key of the node: nothing secret rides on a command line.
#
# Environment:
#   GITAI_ROOT           the root of the deployment (default ~/opt/git-ai)
#   GITAI_IMPORT_KEEP    how many bundles of this repository are kept (5)
#   GITAI_PG_CONTAINER   the container of PostgreSQL (pigsty-postgres)
#   GITAI_PG_PORT        the port PostgreSQL listens on in the container (25432)
#   GITAI_PG_DB          the database of the storage core (gitai)
set -uo pipefail

ROOT="${GITAI_ROOT:-$HOME/opt/git-ai}"
REPO="${1:-}"
REMOTE="${2:-}"
BRANCH="${3:-main}"
KEEP="${GITAI_IMPORT_KEEP:-5}"
PG_CONTAINER="${GITAI_PG_CONTAINER:-pigsty-postgres}"
PG_PORT="${GITAI_PG_PORT:-25432}"
PG_DB="${GITAI_PG_DB:-gitai}"
STAGE="$ROOT/import/remote-staging/$REPO-github.git"
SERVER_REPO="$ROOT/repos/$REPO"

say() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*"; }
die() { say "ERROR: $*"; exit 1; }

# storage_head is the commit the storage core holds for a reference, or
# nothing when the storage core does not hold it.
storage_head() {
  docker exec -u postgres "$PG_CONTAINER" psql -h /run/postgresql -p "$PG_PORT" -d "$PG_DB" -Atc \
    "select r.target from graft_refs r join graft_repositories g on r.repo_id=g.repo_id
     where g.relative_path='$REPO' and convert_from(r.name,'utf-8')='$1'" 2>/dev/null
}

# fetch_objects brings the objects of a reference into the repository of the
# server, without moving any reference: the fetch writes FETCH_HEAD.
fetch_objects() {
  docker exec -e GIT_EXEC_PATH="$ROOT/import/gitbin" \
    -e STAGE="$STAGE" -e SERVER_REPO="$SERVER_REPO" -e BRANCH="$1" \
    git-ai-gitaly "$ROOT/import/gitbin/git" \
    --git-dir="$SERVER_REPO" fetch "$STAGE" "+refs/heads/$BRANCH"
}

# prune_bundles keeps the bundles of the repository that are still of use.
#
# The job used to bundle the delta of every run and leave it behind: a
# directory that grows without end is a directory that fills the disk of the
# node. The bundles of the old runs are of no use any more — the objects are
# in the storage core and the job does not bundle any more — so only the last
# few are kept.
prune_bundles() {
  local keep=0 old
  for old in $(ls -1t "$ROOT/import/delta-$REPO-"*.bundle 2>/dev/null); do
    keep=$((keep + 1))
    [ "$keep" -le "$KEEP" ] && continue
    rm -f "$old"
    say "== removed the old bundle $(basename "$old")"
  done
}

[ -n "$REPO" ] || die "the repository is missing: import-remote.sh <repo.git> [remote-url]"
# The name of the repository goes into a query of the database, so it is
# checked before it is used rather than quoted around.
case "$REPO" in
  *[!A-Za-z0-9._-]*) die "the name of the repository holds a character it may not: $REPO" ;;
esac
if [ -z "$REMOTE" ]; then
  # The default is the GitHub repository of the operator: the name of the
  # repository without its suffix is the name of the repository there.
  REMOTE="ssh://git@ssh.github.com:443/oliveagle/${REPO%.git}.git"
fi
if [ -f "$ROOT/secrets/github_ssh_key" ]; then
  export GIT_SSH_COMMAND="ssh -i $ROOT/secrets/github_ssh_key -o UserKnownHostsFile=$ROOT/secrets/known_hosts -o StrictHostKeyChecking=yes"
fi

export GRAFT_ADDRESS="${GRAFT_ADDRESS:-unix:$ROOT/run/cache.sock}" \
       GRAFT_STORAGE="${GRAFT_STORAGE:-default}" \
       GRAFT_USER="${GRAFT_USER:-oliveagle}" \
       GRAFT_TOKEN="${GRAFT_TOKEN:-$(cat "$ROOT/tokens/shared" 2>/dev/null)}"

mkdir -p "$(dirname "$STAGE")"
if [ ! -d "$STAGE" ]; then
  say "== clone $REMOTE -> $STAGE"
  git clone --bare "$REMOTE" "$STAGE" || exit 2
else
  say "== fetch $REMOTE"
  git -C "$STAGE" fetch --prune origin '+refs/heads/*:refs/heads/*' || exit 2
fi

moved=0
BRANCHES=$(git -C "$STAGE" for-each-ref --format='%(refname:short)' refs/heads/)
for branch in $BRANCHES; do
  remote_head="$(git -C "$STAGE" rev-parse "refs/heads/$branch")"
  storage="$(storage_head "refs/heads/$branch")"
  [ -n "$storage" ] || storage=none
  say "branch $branch: remote=$remote_head storage=$storage"
  if [ "$remote_head" = "$storage" ]; then continue; fi
  if [ "$storage" = none ]; then
    say "ERROR: the storage core does not know $branch; register the repository first"
    exit 3
  fi
  if ! git -C "$STAGE" merge-base --is-ancestor "$storage" "$remote_head"; then
    say "ERROR: the remote $branch is not a descendant of the storage head; a real merge is needed"
    exit 3
  fi

  say "== the objects of $branch into the repository of the server"
  fetch_objects "$branch" || exit 4

  say "== land $remote_head on $branch"
  "$ROOT/bin/graft" land --commit "$remote_head" --strategy ff_only "$branch" --repo "$REPO" || exit 5
  moved=$((moved + 1))
done

if [ "$moved" -eq 0 ]; then
  say "== the storage core already holds the remote state"
else
  say "== $moved reference(s) moved"
  say "== verify"
  "$ROOT/bin/graft" storage verify -repo "$REPO" --json || exit 6
fi

prune_bundles
say "== DONE"
