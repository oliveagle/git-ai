#!/usr/bin/env bash
# mirror-push.sh — publish one storage-core repository to one Git remote, from
# the node host, and prove that the remote carries the commits.
#
# Runs ON the node host:
#
#   mirror-push.sh <repo.git> <git-remote-url>
#
# This is the host-side counterpart of `graft mirror push`. It reads the
# authoritative refs from the storage core (the database that holds them) and
# the objects from the node's local copy, publishes the heads and the tags, and
# then reads the remote back and compares it with what it wanted. A publish
# that cannot be proven is a failed publish, and the run exits non-zero.
#
# Why the host and not the server: the gitaly container has no ssh client,
# and the HTTPS path to a public host is intermittently throttled by that
# host's HTTP/2 limiter on this link. The node host's SSH path is reliable.
#
# The remote is named only by its URL, so a GitHub mirror, a self-hosted one
# and a bare repository on another machine are the same command.
set -euo pipefail

REPO="${1:?usage: mirror-push.sh <repo.git> <git-remote-url>}"
REMOTE="${2:?usage: mirror-push.sh <repo.git> <git-remote-url>}"
ROOT="${GITAI_ROOT:-$HOME/opt/git-ai}"
SRC="$ROOT/repos/$REPO"
MIRROR="$ROOT/mirror/$REPO"
PG_CONTAINER="${PG_CONTAINER:-pigsty-postgres}"
PG_PORT="${PG_PORT:-25432}"
PG_USER="${PG_USER:-temporal}"
PG_DB="${PG_DB:-gitai}"
PG_PASSWORD_SECRET="${PG_PASSWORD_SECRET:-NAS01_DORIS_OLIVEAGLE_PASSWORD}"
PUSH_TIMEOUT="${MIRROR_PUSH_TIMEOUT:-120}"
PUSH_RETRIES="${MIRROR_PUSH_RETRIES:-2}"
PUSH_BACKOFF="${MIRROR_PUSH_BACKOFF:-15}"
PRUNE="${MIRROR_PRUNE:-1}"
export GIT_SSH_COMMAND="${GIT_SSH_COMMAND:-ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=15 -o ServerAliveCountMax=2}"
export GIT_TERMINAL_PROMPT=0

log() { printf '%s %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"; }

[ -d "$SRC" ] || log "the node copy of the repository is missing ($SRC); materializing from the storage core"

# 1. the authoritative refs, from the storage core
export PATH="$HOME/.local/bin:$PATH"
DOTVAULT_BIN="$(command -v dotvault || true)"
[ -n "$DOTVAULT_BIN" ] || DOTVAULT_BIN="$HOME/.local/bin/dotvault"
[ -x "$DOTVAULT_BIN" ] || { log "ERROR: dotvault not found"; exit 1; }
export PGPASSWORD="$(cd "$HOME/.agents" && "$DOTVAULT_BIN" get "$PG_PASSWORD_SECRET")"
psql_() {
  docker exec -e PGPASSWORD="$PGPASSWORD" "$PG_CONTAINER" \
    psql -h 127.0.0.1 -p "$PG_PORT" -U "$PG_USER" -d "$PG_DB" -Atc "$1"
}
REPO_ID="$(psql_ "select repo_id from graft_repositories where relative_path='$REPO'")"
[ -n "$REPO_ID" ] || { log "ERROR: the repository is not in the storage core: $REPO"; exit 1; }
REFS="$(psql_ "select encode(name,'escape') || ' ' || target from graft_refs where repo_id=$REPO_ID and (name like 'refs/heads/%' or name like 'refs/tags/%') and name not like 'refs/heads/graft/%'")"
[ -n "$REFS" ] || { log "ERROR: the storage core holds no ref for $REPO — refusing to publish"; exit 1; }
DEFAULT_BRANCH="$(psql_ "select split_part(target,'/',3) from graft_refs where repo_id=$REPO_ID and encode(name,'escape')='HEAD'")"
DEFAULT_BRANCH="${DEFAULT_BRANCH:-main}"

# 2. the mirror repository: a plain Git repository whose objects are copied
#    from the node copy when the node copy holds it. The objects are immutable,
#    so a copy without the prune flag is right: a repack must not break the mirror.
[ -d "$MIRROR/objects" ] || git init -q --bare --initial-branch="$DEFAULT_BRANCH" "$MIRROR"
if [ -d "$SRC/objects" ]; then
  rsync -a "$SRC/objects/" "$MIRROR/objects/"
fi

# 3. the authoritative refs, as the shape of what the mirror must hold
declare -A WANT
while read -r ref oid; do
  [ -n "$ref" ] || continue
  WANT["$ref"]="$oid"
done <<< "$REFS"

# 4. objects the mirror lacks are fetched through the storage core's own read
#    path. The database and the object bucket are the authority; a node copy is
#    a cache and may be stale, partial or absent — maxllm.git and imp-test.git
#    failed the publish for exactly that reason while `graft storage verify
#    -connectivity` reported them healthy.
MISSING_REFS=()
for ref in "${!WANT[@]}"; do
  git -C "$MIRROR" cat-file -e "${WANT[$ref]}^{object}" 2>/dev/null || MISSING_REFS+=("$ref")
done
if [ "${#MISSING_REFS[@]}" -gt 0 ]; then
  WRAPPER="$ROOT/bin/git-ssh-gitaly"
  [ -x "$WRAPPER" ] || {
    log "ERROR: ${#MISSING_REFS[@]} ref(s) are missing from the mirror and $WRAPPER is not executable — cannot fetch them from the storage core"
    exit 1; }
  log "fetching ${#MISSING_REFS[@]} ref(s) from the storage core: the node copy does not hold them"
  fetch_specs=()
  for ref in "${MISSING_REFS[@]}"; do fetch_specs+=("+$ref:$ref"); done
  GIT_SSH_COMMAND="$WRAPPER" timeout --signal=TERM --kill-after=30 "$PUSH_TIMEOUT" \
    git -C "$MIRROR" fetch --no-tags "ssh://gitaly/$REPO" "${fetch_specs[@]}" ||
    log "WARNING: the fetch from the storage core returned non-zero"
fi


# 5. the authoritative refs, applied to the mirror
for ref in "${!WANT[@]}"; do
  git -C "$MIRROR" update-ref "$ref" "${WANT[$ref]}"
done

# 5b. the refs that vanished from the storage core must leave the mirror too:
#     the destination is a mirror of the authority, not a place of its own.
while read -r ref; do
  [ -n "$ref" ] || continue
  [ -n "${WANT[$ref]:-}" ] || git -C "$MIRROR" update-ref -d "$ref"
done < <(git -C "$MIRROR" for-each-ref --format='%(refname)' refs/heads refs/tags)

# 6. every object a ref names must be there before anything is published
missing=0
while read -r ref oid; do
  [ -n "$ref" ] || continue
  git -C "$MIRROR" cat-file -e "$oid^{object}" 2>/dev/null || {
    log "ERROR: the mirror does not hold the object $oid of $ref"; missing=1; }
done <<< "$REFS"
[ "$missing" -eq 0 ] || { log "ERROR: the object store of the mirror is incomplete — refusing to publish"; exit 1; }
log "refs prepared: ${#WANT[@]} (repository $REPO, id $REPO_ID)"

# 7. publish. Every attempt has a deadline and a retry: the transport must
#    not be able to wedge the schedule.
push_args=(push)
[ "$PRUNE" = "1" ] && push_args+=(--prune)
attempt=1
pushed=0
while [ "$attempt" -le "$PUSH_RETRIES" ]; do
  log "publish attempt $attempt/$PUSH_RETRIES to $REMOTE (timeout ${PUSH_TIMEOUT}s)"
  if timeout --signal=TERM --kill-after=30 "$PUSH_TIMEOUT" \
      git -C "$MIRROR" "${push_args[@]}" "$REMOTE" \
      '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'; then
    pushed=1
    break
  fi
  attempt=$((attempt + 1))
  [ "$attempt" -le "$PUSH_RETRIES" ] && sleep "$PUSH_BACKOFF"
done
[ "$pushed" -eq 1 ] || { log "ERROR: the publish to $REMOTE failed after $PUSH_RETRIES attempts"; exit 1; }
log "published to $REMOTE"

# 8. read the remote back: it must carry exactly the authoritative refs.
REMOTE_REFS="$(timeout --signal=TERM --kill-after=30 "$PUSH_TIMEOUT" git -C "$MIRROR" ls-remote --heads --tags "$REMOTE" || true)"
[ -n "$REMOTE_REFS" ] || { log "ERROR: the remote $REMOTE holds no ref — the publish did not land"; exit 1; }
mismatch=0
for ref in "${!WANT[@]}"; do
  got="$(printf '%s\n' "$REMOTE_REFS" | awk -v r="$ref" '$2 == r {print $1; exit}')"
  if [ "$got" != "${WANT[$ref]}" ]; then
    log "MISMATCH $ref: remote=$got authoritative=${WANT[$ref]}"
    mismatch=$((mismatch + 1))
  fi
done
extra="$(printf '%s\n' "$REMOTE_REFS" | awk 'NF==2 {print $2}' | while read -r r; do
  case "$r" in
    *'^{}') : ;;                    # the peeled form of an annotated tag
    refs/heads/*|refs/tags/*) [ -n "${WANT[$r]:-}" ] || echo "$r" ;;
  esac
done | wc -l)"
if [ "$mismatch" -ne 0 ] || [ "$extra" -ne 0 ]; then
  log "ERROR: the mirror is out of sync — $mismatch wrong, $extra ref(s) on the remote that the storage core does not know"
  exit 1
fi
log "verified: ${#WANT[@]} ref(s) match $REMOTE"
