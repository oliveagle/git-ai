#!/usr/bin/env bash
# local-backup-gate.sh — every local Git repository must have a backup
# origin, and the backup counts only when the origin holds its commits.
#
# Runs on the workstation that keeps the checkouts:
#
#   local-backup-gate.sh [--root DIR]... [--registry FILE] [--repo NAME]
#                        [--sync] [--report FILE] [--quiet]
# The gate walks every root, finds every Git repository, and asks its origin
# what it holds:
#
#   * a repository without an origin is a failure,
#   * an origin that cannot be reached is a failure,
#   * a local commit the origin does not hold is a failure,
#   * uncommitted changes are reported, because a backup of the history
#     cannot hold work that was never committed.
#
# "The origin does not hold it" is decided by asking the origin for the local
# tip, not by comparing two tips. The commits of a branch are backed up when
# the origin can serve its tip, even when the origin does not carry the
# branch under that name any more: a rename or a deleted branch does not
# un-back-up the commits it carried, and an origin that serves another commit
# instead does not hold the local work.
#
# With --sync the gate pushes first and verifies second: a branch whose
# commits the origin does not hold is pushed, and Git itself refuses a push
# that would drop commits, so a branch that diverged is reported instead of
# being forced. The gate never forces a push.
#
# Exit status: 0 when every repository has an origin and every origin holds
# the local commits, 1 otherwise. The report is written as JSON when --report
# names a file.
set -euo pipefail

ROOTS=()
REGISTRY="${BACKUP_GATE_REGISTRY:-$HOME/etc/backup-origins.local.json}"
REPORT=""
SYNC=0
ONLY_REPO=""
QUIET=0
PUSH_TIMEOUT="${BACKUP_PUSH_TIMEOUT:-120}"
export GIT_TERMINAL_PROMPT=0
export GIT_SSH_COMMAND="${GIT_SSH_COMMAND:-ssh -o BatchMode=yes -o ConnectTimeout=15}"

while [ $# -gt 0 ]; do
  case "$1" in
    --registry) REGISTRY="${2:?--registry needs a file}"; shift 2 ;;
    --root)   ROOTS+=("${2:?--root needs a directory}"); shift 2 ;;
    --repo)   ONLY_REPO="${2:?--repo needs a repository}"; shift 2 ;;
    --report) REPORT="${2:?--report needs a file}"; shift 2 ;;
    --sync)   SYNC=1; shift ;;
    --quiet)  QUIET=1; shift ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *)        printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

log() { printf '%s %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"; }
say() { [ "$QUIET" = "1" ] || printf '%s\n' "$*"; }

if [ "${#ROOTS[@]}" -eq 0 ]; then
  ROOTS=("${BACKUP_GATE_ROOT:-/mnt/data/repos/github.com/oliveagle}")
fi
for root in "${ROOTS[@]}"; do
  [ -d "$root" ] || { log "ERROR: the root does not exist: $root"; exit 1; }
done

RESULTS="$(mktemp)"
trap 'rm -f "$RESULTS"' EXIT
FAILED=0

# The registry of the repositories whose origin is a one-way mirror of another
# authority (the storage core of git-ai). A push into such an origin is
# pointless: the mirror prunes what its authority does not know. The gate
# only verifies those repositories and says that the commit has to land in
# the authority.
#
#   {"mirrored": ["laya-workflow", "ole-eval"]}
MIRRORED_LIST=""
if [ -r "$REGISTRY" ]; then
  MIRRORED_LIST="$(python3 - "$REGISTRY" <<'PY'
import json, sys
try:
    with open(sys.argv[1]) as handle:
        registry = json.load(handle)
except Exception as error:  # a broken registry must not hide the check
    print("", file=sys.stderr)
    raise SystemExit(0)
for name in registry.get("mirrored", []):
    print(name)
PY
)"
fi

# is_mirrored reports whether the origin of a repository is a one-way mirror.
is_mirrored() {
  local wanted="$1" name
  while read -r name; do
    [ "$name" = "$wanted" ] && return 0
  done <<< "$MIRRORED_LIST"
  return 1
}

# remote_holds reports whether the origin can serve the given objects. An
# origin that serves the local tip holds the commits of that tip, whatever
# name it gives the branch.
remote_holds() {
  local repo_dir="$1" url="$2"
  shift 2
  timeout --signal=TERM --kill-after=20 "$PUSH_TIMEOUT" \
    git -C "$repo_dir" fetch --no-tags --quiet "$url" "$@" >/dev/null 2>&1
}

MISSING_LIST=()
UNNAMED_LIST=()

# classify decides, for one repository, which of its commits the origin does
# not hold. The branches whose name the origin does not carry any more, but
# whose commits it holds, are listed apart: they are backed up.
classify() {
  local repo_dir="$1" url="$2" remote_refs="$3" branches="$4" tags="$5"
  MISSING_LIST=()
  UNNAMED_LIST=()
  local oid name got batch="yes"
  local -a oids=()
  while read -r oid name; do
    [ -n "$name" ] || continue
    oids+=("$oid")
  done <<< "$branches"
  while read -r oid name; do
    [ -n "$name" ] || continue
    oids+=("$oid")
  done <<< "$tags"
  # One round trip for the whole set; a failure only means that one of the
  # tips is missing, so the tips are then asked for one by one.
  if [ "${#oids[@]}" -gt 0 ] && ! remote_holds "$repo_dir" "$url" "${oids[@]}"; then
    batch="no"
  fi
  while read -r oid name; do
    [ -n "$name" ] || continue
    got="$(printf '%s\n' "$remote_refs" | awk -v r="refs/heads/$name" '$2 == r {print $1; exit}')"
    [ "$got" = "$oid" ] && continue
    if [ "$batch" = "no" ] && ! remote_holds "$repo_dir" "$url" "$oid"; then
      MISSING_LIST+=("heads/$name")
      continue
    fi
    [ -n "$got" ] || UNNAMED_LIST+=("heads/$name")
  done <<< "$branches"
  while read -r oid name; do
    [ -n "$name" ] || continue
    got="$(printf '%s\n' "$remote_refs" | awk -v r="refs/tags/$name" '$2 == r {print $1; exit}')"
    [ "$got" = "$oid" ] && continue
    if [ "$batch" = "no" ] && ! remote_holds "$repo_dir" "$url" "$oid"; then
      MISSING_LIST+=("tags/$name")
      continue
    fi
    [ -n "$got" ] || UNNAMED_LIST+=("tags/$name")
  done <<< "$tags"
}

# The repositories: every directory under the root that holds a .git entry.
# A worktree has a .git file, a clone a .git directory; both are repositories.
while IFS= read -r dotgit; do
  repo_dir="$(dirname "$dotgit")"
  name="$(basename "$repo_dir")"
  [ -n "$ONLY_REPO" ] && [ "$name" != "$ONLY_REPO" ] && continue

  # A .git entry that does not lead to a repository is a broken checkout (a
  # submodule whose git directory moved away, for instance). It holds no work
  # and it is not a repository any more, so it is named and skipped.
  if ! git -C "$repo_dir" rev-parse --git-dir >/dev/null 2>&1; then
    say "note  $name: the checkout is broken ($dotgit does not lead to a repository)"
    continue
  fi

  url="$(git -C "$repo_dir" remote get-url origin 2>/dev/null || true)"
  if [ -z "$url" ]; then
    say "FAIL  $name: the repository has no origin"
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "" "$repo_dir" "no_origin" "the repository has no origin" "0" >>"$RESULTS"
    FAILED=1
    continue
  fi

  if ! REMOTE_REFS="$(timeout --signal=TERM --kill-after=20 "$PUSH_TIMEOUT" \
      git -C "$repo_dir" ls-remote --heads --tags "$url" 2>/dev/null)"; then
    say "FAIL  $name: the origin $url cannot be reached"
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "origin" "$url" "unreachable" "the origin cannot be reached" "0" >>"$RESULTS"
    FAILED=1
    continue
  fi

  LOCAL_BRANCHES="$(git -C "$repo_dir" for-each-ref --format='%(objectname) %(refname:short)' refs/heads)"
  LOCAL_TAGS="$(git -C "$repo_dir" for-each-ref --format='%(objectname) %(refname:short)' refs/tags)"

  classify "$repo_dir" "$url" "$REMOTE_REFS" "$LOCAL_BRANCHES" "$LOCAL_TAGS"
  MIRRORED="no"; is_mirrored "$name" && MIRRORED="yes"

  if [ "$SYNC" = "1" ] && [ "${#MISSING_LIST[@]}" -gt 0 ] && [ "$MIRRORED" = "no" ]; then
    for entry in "${MISSING_LIST[@]}"; do
      case "$entry" in
        heads/*) local_ref="refs/heads/${entry#heads/}" ;;
        tags/*)  local_ref="refs/tags/${entry#tags/}" ;;
        *)       continue ;;
      esac
      if timeout --signal=TERM --kill-after=20 "$PUSH_TIMEOUT" \
          git -C "$repo_dir" push origin "$local_ref:$local_ref" >/dev/null 2>&1; then
        log "pushed $name/$local_ref to $url"
      else
        log "ERROR: the push of $name/$local_ref to $url was refused; the branches diverged and the gate does not force a push"
      fi
    done
    REMOTE_REFS="$(timeout --signal=TERM --kill-after=20 "$PUSH_TIMEOUT" \
      git -C "$repo_dir" ls-remote --heads --tags "$url" 2>/dev/null || true)"
    classify "$repo_dir" "$url" "$REMOTE_REFS" "$LOCAL_BRANCHES" "$LOCAL_TAGS"
  fi

  uncommitted="$(git -C "$repo_dir" status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
  branches="$(printf "%s\n" "$LOCAL_BRANCHES" | awk "NF" | wc -l | tr -d " ")"
  tags="$(printf "%s\n" "$LOCAL_TAGS" | awk "NF" | wc -l | tr -d " ")"

  if [ "${#MISSING_LIST[@]}" -gt 0 ]; then
    missing="${MISSING_LIST[*]}"
    say "FAIL  $name: the origin $url does not hold:$missing"
    [ "$MIRRORED" = "yes" ] && say "      $name is mirrored from another authority: land the commit there, the mirror prunes it"

    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "origin" "$url" "out_of_sync" "the origin does not hold:$missing" "$uncommitted" >>"$RESULTS"
    FAILED=1
  else
    say "ok    $name: $branches branch(es) and $tags tag(s) are held by $url"
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$name" "origin" "$url" "in_sync" "$branches branches and $tags tags are held by the origin" "$uncommitted" >>"$RESULTS"
    if [ "${#UNNAMED_LIST[@]}" -gt 0 ]; then
      say "note  $name: the origin holds the commits of ${UNNAMED_LIST[*]} but does not carry them under that name"
    fi
    if [ "$uncommitted" != "0" ]; then
      say "note  $name: $uncommitted uncommitted change(s) are in the working tree; a backup of the history cannot hold them"
    fi
  fi
done < <(for root in "${ROOTS[@]}"; do find "$root" -maxdepth 3 -name .git; done | sort)

if [ -n "$REPORT" ]; then
  mkdir -p "$(dirname "$REPORT")"
  python3 - "$RESULTS" "$REPORT" "$FAILED" "${ROOTS[*]}" <<'PY'
import json, sys, datetime

rows, path, failed, roots = sys.argv[1], sys.argv[2], sys.argv[3] == "1", sys.argv[4]
repositories = []
with open(rows) as handle:
    for line in handle:
        parts = line.rstrip("\n").split("\t") + [""] * 6
        repository, origin, url, status, detail, uncommitted = parts[:6]
        repositories.append({
            "repository": repository,
            "origin": origin,
            "url": url,
            "status": status,
            "detail": detail,
            "uncommitted": int(uncommitted or 0),
        })
report = {
    "checked_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "roots": roots.split(),
    "result": "FAIL" if failed else "PASS",
    "repositories": repositories,
}
with open(path, "w") as handle:
    json.dump(report, handle, indent=1)
    handle.write("\n")
PY
fi

if [ "$FAILED" -ne 0 ]; then
  log "the gate refused: at least one repository has no backup origin that holds its commits"
  exit 1
fi
log "the gate passed: every repository has a backup origin that holds its commits"
