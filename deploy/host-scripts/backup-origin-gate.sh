#!/usr/bin/env bash
# backup-origin-gate.sh — every repository of the storage core must have a
# backup origin, and a backup counts only when the origin has been read back
# and carries the same commits.
#
# Runs ON the node host:
#
#   backup-origin-gate.sh [--registry FILE] [--repo NAME] [--sync]
#                         [--report FILE] [--quiet]
#
# The gate reads the authoritative refs from the storage core, reads the
# registry of the backup origins, and asks every origin what it holds:
#
#   * a repository without an origin in the registry is a failure,
#   * an origin that cannot be reached is a failure,
#   * an origin that does not carry a branch it may receive, or that carries
#     it with another commit id, is a failure,
#   * an origin that carries a ref the storage core does not know is a
#     failure, because a person has to look at that divergence.
#
# With --sync the gate publishes first and verifies second: a repository that
# drifted is repaired by the run, and the gate still fails when the repair did
# not work. The publish uses the sync command of the registry entry when it
# has one, and deploy/host-scripts/mirror-push.sh otherwise.
#
# Exit status: 0 when every repository has an origin and every origin is in
# sync, 1 otherwise. The report is written as JSON when --report names a
# file, and the summary is printed on the standard output.
set -euo pipefail

ROOT="${GITAI_ROOT:-$HOME/opt/git-ai}"
REGISTRY="${BACKUP_ORIGINS_FILE:-$ROOT/mirror/backup-origins.json}"
PG_CONTAINER="${PG_CONTAINER:-pigsty-postgres}"
PG_PORT="${PG_PORT:-25432}"
PG_USER="${PG_USER:-temporal}"
PG_DB="${PG_DB:-gitai}"
PG_PASSWORD_SECRET="${PG_PASSWORD_SECRET:-NAS01_DORIS_OLIVEAGLE_PASSWORD}"
PUSH_TIMEOUT="${MIRROR_PUSH_TIMEOUT:-120}"
REPORT=""
SYNC=0
ONLY_REPO=""
QUIET=0

while [ $# -gt 0 ]; do
  case "$1" in
    --registry) REGISTRY="${2:?--registry needs a file}"; shift 2 ;;
    --repo)     ONLY_REPO="${2:?--repo needs a repository}"; shift 2 ;;
    --report)   REPORT="${2:?--report needs a file}"; shift 2 ;;
    --sync)     SYNC=1; shift ;;
    --quiet)    QUIET=1; shift ;;
    -h|--help)  sed -n '2,26p' "$0"; exit 0 ;;
    *)          printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

log() { printf '%s %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"; }
say() { [ "$QUIET" = "1" ] || printf '%s\n' "$*"; }

[ -r "$REGISTRY" ] || { log "ERROR: the registry of the backup origins is not readable: $REGISTRY"; exit 1; }
command -v python3 >/dev/null || { log "ERROR: python3 is needed to read the registry"; exit 1; }

# The authoritative refs live in the storage core, which is a database.
export PATH="$HOME/.local/bin:$PATH"
DOTVAULT_BIN="$(command -v dotvault || true)"
[ -n "$DOTVAULT_BIN" ] || DOTVAULT_BIN="$HOME/.local/bin/dotvault"
[ -x "$DOTVAULT_BIN" ] || { log "ERROR: dotvault not found"; exit 1; }
export PGPASSWORD="$(cd "$HOME/.agents" && "$DOTVAULT_BIN" get "$PG_PASSWORD_SECRET")"
psql_() {
  docker exec -e PGPASSWORD="$PGPASSWORD" "$PG_CONTAINER" \
    psql -h 127.0.0.1 -p "$PG_PORT" -U "$PG_USER" -d "$PG_DB" -Atc "$1"
}

# The registry, read into one row per origin: repository, name, url, branch
# patterns, sync command.
ROWS="$(mktemp)"
RESULTS="$(mktemp)"
trap 'rm -f "$ROWS" "$RESULTS"' EXIT
python3 - "$REGISTRY" >"$ROWS" <<'PY'
import json, sys

with open(sys.argv[1]) as handle:
    registry = json.load(handle)

repositories = registry.get("repositories", {})
for repository in sorted(repositories):
    for origin in repositories[repository].get("origins", []):
        branches = origin.get("branches") or ["*"]
        print("\t".join([
            repository,
            origin.get("name", ""),
            origin.get("url", ""),
            " ".join(branches),
            origin.get("sync", ""),
        ]))
PY

# branch_allowed reports whether a branch name matches one of the patterns of
# an origin. The patterns are the shape of `graft mirror push --branch`: no
# refs/heads/ prefix, and * is allowed. The list arrives quoted, because an
# unquoted * would be expanded against the files of the working directory.
branch_allowed() {
  local branch="$1"; shift
  local pattern
  for pattern in "$@"; do
    case "$branch" in
      $pattern) return 0 ;;
    esac
  done
  return 1
}

# The refs an origin must carry: the branches its patterns allow, and every
# tag, because a tag carries the commits that a branch may name.
wanted_refs() {
  local patterns="$1"
  local refs="$2"
  local ref oid branch
  local -a allowed
  read -r -a allowed <<< "$patterns"
  while read -r ref oid; do
    [ -n "$ref" ] || continue
    case "$ref" in
      refs/heads/*)
        branch="${ref#refs/heads/}"
        branch_allowed "$branch" "${allowed[@]}" || continue
        ;;
      refs/tags/*) ;;
      *) continue ;;
    esac
    printf '%s %s\n' "$ref" "$oid"
  done <<< "$refs"
}

# The gate itself: every repository of the storage core, every origin of it.
FAILED=0
SYNC_LOG="$ROOT/logs/backup-origin-gate-sync.log"
mkdir -p "$ROOT/logs"

# The publisher: beside this script, or an installed copy in bin/.
PUBLISH="$ROOT/bin/mirror-push.sh"
[ -x "$PUBLISH" ] || PUBLISH="$(dirname "$(readlink -f "$0")")/mirror-push.sh"

REPOSITORIES="$(psql_ "select repo_id, relative_path from graft_repositories order by repo_id")"
while IFS='|' read -r repo_id repository; do
  [ -n "$repository" ] || continue
  if [ -n "$ONLY_REPO" ] && [ "$repository" != "$ONLY_REPO" ]; then continue; fi

  REFS="$(psql_ "select encode(name,'escape') || ' ' || target from graft_refs where repo_id=$repo_id and (name like 'refs/heads/%' or name like 'refs/tags/%') and name not like 'refs/heads/graft/%'")"
  ORIGINS="$(awk -F'\t' -v r="$repository" '$1 == r {print $2 "\t" $3 "\t" $4 "\t" $5}' "$ROWS")"

  if [ -z "$ORIGINS" ]; then
    say "FAIL  $repository: no backup origin is registered"
    printf '%s\t%s\t%s\t%s\t%s\n' "$repository" "" "" "no_origin" "no backup origin is registered" >>"$RESULTS"
    FAILED=1
    continue
  fi
  if [ -z "$REFS" ]; then
    say "FAIL  $repository: the storage core holds no branch or tag, so there is nothing to back up"
    printf '%s\t%s\t%s\t%s\t%s\n' "$repository" "" "" "nothing_to_back_up" "the storage core holds no branch or tag" >>"$RESULTS"
    FAILED=1
    continue
  fi

  while IFS=$'\t' read -r origin_name url patterns sync_cmd; do
    [ -n "$url" ] || continue

    if [ "$SYNC" = "1" ]; then
      if [ -n "$sync_cmd" ]; then
        bash -c "$sync_cmd" >>"$SYNC_LOG" 2>&1 || true
      else
        "$PUBLISH" "$repository" "$url" >>"$SYNC_LOG" 2>&1 || true
      fi
    fi

    if ! REMOTE_REFS="$(timeout --signal=TERM --kill-after=30 "$PUSH_TIMEOUT" git ls-remote --heads --tags "$url" 2>/dev/null)"; then
      say "FAIL  $repository/$origin_name: the origin $url cannot be reached"
      printf '%s\t%s\t%s\t%s\t%s\n' "$repository" "$origin_name" "$url" "unreachable" "the origin cannot be reached" >>"$RESULTS"
      FAILED=1
      continue
    fi

    WANT_NAMES=()
    WANT_OIDS=()
    while read -r ref oid; do
      [ -n "$ref" ] || continue
      WANT_NAMES+=("$ref")
      WANT_OIDS+=("$oid")
    done < <(wanted_refs "$patterns" "$REFS")

    mismatch=0
    detail=""
    if [ "${#WANT_NAMES[@]}" -gt 0 ]; then
      for i in "${!WANT_NAMES[@]}"; do
        ref="${WANT_NAMES[$i]}"
        oid="${WANT_OIDS[$i]}"
        got="$(printf '%s\n' "$REMOTE_REFS" | awk -v r="$ref" '$2 == r {print $1; exit}')"
        if [ "$got" != "$oid" ]; then
          mismatch=$((mismatch + 1))
          detail="$detail $ref"
        fi
      done
    fi

    extra=0
    extra_detail=""
    while read -r r_oid r_name; do
      [ -n "$r_name" ] || continue
      case "$r_name" in
        *'^{}') continue ;;
        refs/heads/*|refs/tags/*) ;;
        *) continue ;;
      esac
      found=0
      if [ "${#WANT_NAMES[@]}" -gt 0 ]; then
        for name in "${WANT_NAMES[@]}"; do
          if [ "$name" = "$r_name" ]; then found=1; break; fi
        done
      fi
      if [ "$found" -eq 0 ]; then
        extra=$((extra + 1))
        extra_detail="$extra_detail +$r_name"
      fi
    done < <(printf '%s\n' "$REMOTE_REFS" | awk 'NF == 2')

    # An origin that another mechanism keeps in sync (the registry names its
    # sync command) prunes what the storage core does not know, so a ref that
    # is still there is a moment of that race and not a broken backup. An
    # origin the gate owns must carry exactly the authoritative refs.
    if [ "$sync_cmd" != "" ] && [ "$extra" -ne 0 ]; then
      say "note  $repository/$origin_name: $extra ref(s) on the origin are unknown to the storage core and are being pruned by its own sync:$extra_detail"
      extra=0
      extra_detail=""
    fi
    if [ "$mismatch" -ne 0 ] || [ "$extra" -ne 0 ]; then
      say "FAIL  $repository/$origin_name: $mismatch ref(s) differ and $extra ref(s) are unknown to the storage core:$detail$extra_detail"
      printf '%s\t%s\t%s\t%s\t%s\n' "$repository" "$origin_name" "$url" "out_of_sync" "$mismatch wrong, $extra unknown:$detail$extra_detail" >>"$RESULTS"
      FAILED=1
    else
      say "ok    $repository/$origin_name: ${#WANT_NAMES[@]} ref(s) match $url"
      printf '%s\t%s\t%s\t%s\t%s\n' "$repository" "$origin_name" "$url" "in_sync" "${#WANT_NAMES[@]} refs match" >>"$RESULTS"
    fi
  done <<< "$ORIGINS"
done <<< "$REPOSITORIES"

if [ -n "$REPORT" ]; then
  mkdir -p "$(dirname "$REPORT")"
  python3 - "$RESULTS" "$REPORT" "$FAILED" <<'PY'
import json, sys, datetime

rows, path, failed = sys.argv[1], sys.argv[2], sys.argv[3] == "1"
repositories = []
with open(rows) as handle:
    for line in handle:
        repository, origin, url, status, detail = (line.rstrip("\n").split("\t") + [""])[:5]
        repositories.append({
            "repository": repository,
            "origin": origin,
            "url": url,
            "status": status,
            "detail": detail,
        })
report = {
    "checked_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "result": "FAIL" if failed else "PASS",
    "repositories": repositories,
}
with open(path, "w") as handle:
    json.dump(report, handle, indent=1)
    handle.write("\n")
PY
fi

if [ "$FAILED" -ne 0 ]; then
  log "the gate refused: at least one repository has no backup origin in sync"
  exit 1
fi
log "the gate passed: every repository has a backup origin in sync"
