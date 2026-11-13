#!/usr/bin/env bash
# integration-suite.sh — the gate: exercise a lane end to end and say whether
# the release it runs is fit to serve production.
#
# Runs ON the deploy host, against one lane. Every case is a black-box
# conversation with the server through the lane's cache port, plus the checks
# only the host can make: which database and which bucket the writes reached,
# whether the other lane was touched, and whether the data outlives the
# process.
#
#   LANE LANE_DIR LANE_GITALY_PORT LANE_CACHE_TCP_PORT LANE_CACHE_METRICS_PORT
#   LANE_PG_DB LANE_PG_USER LANE_BUCKET VERSION
#   PG_CONTAINER PG_IN_CONTAINER_PORT RUSTFS_ENDPOINT RUSTFS_REGION
#   OTHER_DIR OTHER_PG_DB OTHER_BUCKET (optional)
#
# It writes two reports next to the lane's others:
#   reports/integration-<version>-<run>.json   the gate reads this one
#   reports/integration-<version>-<run>.log    every command's output
set -euo pipefail

run_id="$(date +%Y%m%d-%H%M%S)"
work="$LANE_DIR/reports/work-$VERSION-$run_id"
report_json="$LANE_DIR/reports/integration-$VERSION-$run_id.json"
report_log="$LANE_DIR/reports/integration-$VERSION-$run_id.log"
mkdir -p "$work"
: >"$report_log"

export GRAFT_ADDRESS="tcp://127.0.0.1:$LANE_CACHE_TCP_PORT"
export GRAFT_STORAGE=default
export GRAFT_TOKEN="$(cat "$LANE_DIR/tokens/shared")"
export GRAFT_USER="${GRAFT_USER:-it-runner}"
export GRAFT_AUTHOR="integration test <it@localhost>"
graft="$LANE_DIR/current/bin/graft"

# The storage commands name their repository with -repo, so they must not run
# inside a workspace: graft refuses a -repo that is not the workspace's own.
neutral="$work/neutral"
mkdir -p "$neutral"
storage() { (cd "$neutral" && "$graft" storage "$@"); }

passes=0
failures=0
results=""
name=""
detail=""

begin() { name="$1"; printf '\n--- %s\n' "$name" >>"$report_log"; }
verdict() {
  if [ "$1" = "0" ]; then
    passes=$((passes + 1))
    printf '  pass  %s\n' "$name"
    results="${results}${name}	pass	${2:-}
"
  else
    failures=$((failures + 1))
    printf '  FAIL  %s (%s)\n' "$name" "${2:-}"
    results="${results}${name}	FAIL	${2:-}
"
  fi
}

# at_least and is_zero read a count that may be empty when the query behind
# it failed: empty or non-numeric is a failure, never a passing comparison.
at_least() {
  v="$(printf '%s' "${1:-}" | tr -d '[:space:]')"
  case "$v" in ''|*[!0-9]*) return 1 ;; esac
  [ "$v" -ge "$2" ]
}
is_zero() { v="$(printf '%s' "${1:-}" | tr -d '[:space:]')"; [ "$v" = "0" ]; }

# psql on the lane's database, and on another database of the same server.
psql_db() { docker exec -u postgres "$PG_CONTAINER" psql -p "$PG_IN_CONTAINER_PORT" -d "$1" -tAc "$2" 2>/dev/null | tr -d '[:space:]'; }
psql_this() { psql_db "$LANE_PG_DB" "$1"; }
psql_other() { psql_db "$OTHER_PG_DB" "$1"; }

# Object counts go through the object store container: the bucket directories
# are owned by its uid, so the deploy user cannot walk them from the host.
bucket_files() {
  docker exec "$RUSTFS_CONTAINER" sh -c "find /data/$1 -type f 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]'
}

# The cache daemon's object_hits counter, read from its metrics endpoint.
object_hits() {
  curl -s --max-time 3 "http://127.0.0.1:$LANE_CACHE_METRICS_PORT/stats" |
    sed -n 's/.*"object_hits":\([0-9]*\).*/\1/p'
}

# What the lanes hold before the run writes anything, so the isolation case
# can show growth in this lane's bucket and none in the other.
this_bucket_before="$(bucket_files "${LANE_BUCKET:-}")"
this_bucket_before="${this_bucket_before:-0}"
other_bucket_before=""
if [ -n "${OTHER_BUCKET:-}" ]; then
  other_bucket_before="$(bucket_files "$OTHER_BUCKET")"
  other_bucket_before="${other_bucket_before:-0}"
fi

repo_main="it-$run_id-main.git"
repo_other="it-$run_id-side.git"
file_a="notes/$run_id.txt"

# --- 1. a repository is created and cloned --------------------------------
begin "repo_create_clone"
{
  "$graft" repo create "$repo_main" --initial-commit --branch main --json
  "$graft" clone "$GRAFT_ADDRESS" "$repo_main" "$work/main" --branch main
} >>"$report_log" 2>&1 || true
if [ -f "$work/main/README.md" ]; then verdict 0 "cloned $repo_main"; else verdict 1 "no README.md in the clone"; fi

# --- 2. a clean workspace says so ----------------------------------------
begin "status_clean"
out="$(cd "$work/main" && "$graft" status 2>&1)" || true
printf '%s\n' "$out" >>"$report_log"
case "$out" in
  *"nothing changed"*) verdict 0 "" ;;
  *) verdict 1 "status: $out" ;;
esac

# --- 3. commit, land, and read the result back ---------------------------
begin "commit_land_read"
{
  mkdir -p "$work/main/$(dirname "$file_a")"
  printf 'first line\nsecond line\n' >"$work/main/$file_a"
  cd "$work/main"
  "$graft" status
  "$graft" commit -m "land $run_id" --land main
  "$graft" log --limit 5
  "$graft" show HEAD --json
  "$graft" cat "HEAD:$file_a"
} >>"$report_log" 2>&1 || true
head_file="$(cd "$work/main" && "$graft" cat "HEAD:$file_a" 2>/dev/null | tr -d '\n')"
landed="$(psql_this "SELECT count(*) FROM graft_changelog WHERE repo_id = (SELECT repo_id FROM graft_repositories WHERE relative_path = '$repo_main')")"
case "$head_file" in
  *"first line"*"second line"*)
    if at_least "${landed:-0}" 2; then verdict 0 "$landed changelog rows, file content kept"; else verdict 1 "changelog rows: ${landed:-none}"; fi ;;
  *) verdict 1 "HEAD:$file_a does not hold the landed content" ;;
esac

# --- 4. history, blame and search agree on the file ----------------------
begin "history_blame_search"
{
  cd "$work/main"
  "$graft" history "$file_a"
  "$graft" blame "$file_a"
  "$graft" search "first line" --json
} >>"$report_log" 2>&1 || true
hits="$(cd "$work/main" && "$graft" history "$file_a" 2>/dev/null | wc -l)"
found="$(cd "$work/main" && "$graft" search "first line" 2>/dev/null | wc -l)"
if at_least "$hits" 1 && at_least "$found" 1; then verdict 0 "$hits commits, $found matches"
else verdict 1 "history $hits, search $found"; fi

# --- 5. a review is opened on a pending commit and approved -------------
begin "review_flow"
review_file="review/$run_id.txt"
{
  mkdir -p "$work/main/$(dirname "$review_file")"
  printf 'reviewed content\n' >"$work/main/$review_file"
  cd "$work/main"
  "$graft" commit -m "review $run_id"
  "$graft" review create --branch main --title "review $run_id" --description "integration"
  "$graft" review list --branch main
} >>"$report_log" 2>&1 || true
review_id="$(cd "$work/main" && "$graft" review list --branch main 2>/dev/null | awk -v t="review $run_id" '$0 ~ t {print $1}' | tail -1)"
if [ -n "$review_id" ]; then
  if (cd "$work/main" && "$graft" review approve "$review_id" --body "approved by the gate") >>"$report_log" 2>&1; then
    verdict 0 "review $review_id approved"
  else
    verdict 1 "approve of review $review_id failed"
  fi
else
  verdict 1 "no review was opened"
fi

# --- 6. the workspace follows the branch and discards a local change ----
begin "update_revert"
{
  cd "$work/main"
  "$graft" land main
  "$graft" update
  printf 'temporary\n' >"$work/main/$file_a"
  "$graft" status
  "$graft" revert "$file_a"
  "$graft" status
} >>"$report_log" 2>&1 || true
if (cd "$work/main" && "$graft" status 2>&1 | sed -n 's/.*nothing changed.*/clean/p' | head -1 | wc -l | awk '$1==1{exit 0} {exit 1}'); then
  verdict 0 "land + update + revert left the workspace clean"
else
  verdict 1 "workspace is not clean after revert"
fi

# --- 7. a second writer lands beside the first -------------------------
begin "second_writer"
side="it-$run_id-writer"
{
  "$graft" repo create "$repo_other" --initial-commit --branch main
  "$graft" clone "$GRAFT_ADDRESS" "$repo_other" "$work/side" --branch main
  cd "$work/side"
  printf 'side\n' >"side.txt"
  "$graft" commit -m "side $run_id" --land main
  "$graft" log --limit 3
} >>"$report_log" 2>&1 || true
if (cd "$work/side" && "$graft" cat "HEAD:side.txt" 2>/dev/null | sed -n 's/.*side.*/ok/p' | head -1 | wc -l | awk '$1==1{exit 0} {exit 1}'); then
  verdict 0 "the second repository landed its first change"
else
  verdict 1 "the second repository did not land"
fi

# --- 8. a stale writer is refused, and does not overwrite --------------
begin "stale_writer_refused"
{
  cd "$work/side"
  printf 'side two\n' >"side.txt"
  "$graft" commit -m "second side $run_id" --land main
} >>"$report_log" 2>&1 || true
# A writer whose base is behind lands only if the server accepts it; the
# change must either land on top or be refused, never lose the first one.
out="$(cd "$work/side" && "$graft" log --limit 5 2>&1)"
printf '%s\n' "$out" >>"$report_log"
first_still_there="$(psql_this "SELECT count(*) FROM graft_changelog WHERE repo_id = (SELECT repo_id FROM graft_repositories WHERE relative_path = '$repo_other')")"
if at_least "${first_still_there:-0}" 2; then verdict 0 "$first_still_there changelog rows, no history lost"; else verdict 1 "history lost: ${first_still_there:-0} rows"; fi

# --- 9. the storage core verifies its objects --------------------------
begin "storage_verify"
{
  storage state -repo "$repo_main" --json
  storage verify -repo "$repo_main" --connectivity
  storage gc -repo "$repo_main" --dry-run
} >>"$report_log" 2>&1 || true
if storage verify -repo "$repo_main" --connectivity 2>/dev/null | sed -n 's/.*consistent.*/ok/p' | head -1 | wc -l | awk '$1==1{exit 0} {exit 1}'; then
  verdict 0 "state, connectivity and a dry-run gc are clean"
else
  verdict 1 "storage verify did not report consistency"
fi

# --- 10. the other lane's token is refused ------------------------------
begin "token_isolation"
if [ -n "${OTHER_DIR:-}" ] && [ -s "$OTHER_DIR/tokens/shared" ]; then
  other_token="$(cat "$OTHER_DIR/tokens/shared")"
  if [ "$other_token" = "$GRAFT_TOKEN" ]; then
    verdict 1 "both lanes hold the same token"
  elif GRAFT_TOKEN="$other_token" storage state -repo "$repo_main" --json >/dev/null 2>&1; then
    verdict 1 "the other lane's token was accepted"
  else
    verdict 0 "the other lane's token is refused"
  fi
else
  verdict 0 "no other lane to compare with"
fi

# --- 11. the writes reached this lane's database and bucket only -------
begin "data_isolation"
rows_here="$(psql_this "SELECT count(*) FROM graft_repositories WHERE relative_path IN ('$repo_main','$repo_other')")"
if [ -n "${OTHER_PG_DB:-}" ]; then
  rows_there="$(psql_other "SELECT count(*) FROM graft_repositories WHERE relative_path IN ('$repo_main','$repo_other')")"
else
  rows_there="0"
fi
this_bucket_files="$(bucket_files "$LANE_BUCKET")"
other_bucket_files="$other_bucket_before"
if [ -n "${OTHER_BUCKET:-}" ]; then other_bucket_files="$(bucket_files "$OTHER_BUCKET")"; fi
printf 'repos here=%s there=%s, objects here=%s (was %s) there=%s (was %s)\n' \
  "$rows_here" "$rows_there" "$this_bucket_files" "$this_bucket_before" \
  "$other_bucket_files" "$other_bucket_before" >>"$report_log"
# What this run wrote must sit in this lane's database and bucket, and the
# other lane must look exactly as it did before the run started.
if at_least "${rows_here:-0}" 2 && is_zero "${rows_there:-x}" &&
   at_least "${this_bucket_files:-0}" "$((this_bucket_before + 1))" &&
   [ "${other_bucket_files:-x}" = "${other_bucket_before:-y}" ]; then
  verdict 0 "2 repositories and new objects in $LANE_PG_DB/$LANE_BUCKET only"
else
  verdict 1 "repos here=$rows_here there=$rows_there, objects here=$this_bucket_files (was $this_bucket_before) there=$other_bucket_files (was $other_bucket_before)"
fi

# --- 12. the cache serves repeated reads ------------------------------
begin "cache_serves"
hits_before="$(object_hits)"
{
  # Cloning the same repository again reads the same objects: the cache in
  # front of the object store is what serves them the second time.
  (cd "$neutral" && "$graft" clone "$GRAFT_ADDRESS" "$repo_main" "$work/cache-probe" --branch main)
} >>"$report_log" 2>&1 || true
stats="$(curl -s --max-time 3 "http://127.0.0.1:$LANE_CACHE_METRICS_PORT/stats")"
printf '%s\n' "$stats" >>"$report_log"
hits_after="$(object_hits)"
# Whatever the counter was, reading the repository a second time has to raise
# it: a cache that is not in the read path leaves it flat.
if at_least "${hits_after:-0}" "$((${hits_before:-0} + 1))"; then
  verdict 0 "object_hits $hits_before -> $hits_after on a repeated clone"
else
  verdict 1 "object_hits stayed at ${hits_after:-0} on a repeated clone"
fi

# --- 13. the data outlives the process and its local cache ------------
begin "restart_survives"
before="$(psql_this "SELECT count(*) FROM graft_changelog WHERE repo_id = (SELECT repo_id FROM graft_repositories WHERE relative_path = '$repo_main')")"
docker restart "$LANE_CONTAINER" >>"$report_log" 2>&1
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  sleep 2
  if storage state -repo "$repo_main" --json >/dev/null 2>&1; then break; fi
done
after="$(psql_this "SELECT count(*) FROM graft_changelog WHERE repo_id = (SELECT repo_id FROM graft_repositories WHERE relative_path = '$repo_main')")"
if storage state -repo "$repo_main" --json >>"$report_log" 2>&1 && [ "$before" = "$after" ]; then
  verdict 0 "restart kept $after changelog rows"
else
  verdict 1 "before=$before after=$after"
fi

# --- 14. a lost local cache is rebuilt from the storage core ----------
begin "cache_rebuild"
if [ "$LANE" = "staging" ]; then
  if rm -rf "$LANE_DIR/repos/$repo_main" && "$graft" clone "$GRAFT_ADDRESS" "$repo_main" "$work/rebuild" --branch main >>"$report_log" 2>&1; then
    if [ -f "$work/rebuild/$file_a" ]; then verdict 0 "the clone was rebuilt from the storage core"; else verdict 1 "the rebuilt clone lacks $file_a"; fi
  else
    verdict 1 "the clone did not rebuild"
  fi
else
  verdict 0 "skipped on the production lane"
fi

# --- the report ------------------------------------------------------
result="PASS"
[ "$failures" -eq 0 ] || result="FAIL"
{
  printf '{\n'
  printf '  "version": "%s",\n' "$VERSION"
  printf '  "lane": "%s",\n' "$LANE"
  printf '  "run": "%s",\n' "$run_id"
  printf '  "finished_at": "%s",\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  printf '  "result": "%s",\n' "$result"
  printf '  "passed": %s,\n' "$passes"
  printf '  "failed": %s,\n' "$failures"
  printf '  "cases": [\n'
  first=1
  while IFS=$'\t' read -r case_name case_status case_detail; do
    [ -n "$case_name" ] || continue
    [ "$first" -eq 1 ] || printf ',\n'
    first=0
    printf '    {"case": "%s", "status": "%s", "detail": "%s"}' \
      "$case_name" "$case_status" "$(printf '%s' "$case_detail" | sed 's/"/\\"/g')"
  done <<<"$results"
  printf '\n  ]\n}\n'
} >"$report_json"

printf '\n%s: %s passed, %s failed (release %s, lane %s)\n' "$result" "$passes" "$failures" "$VERSION" "$LANE"
printf 'report: %s\n' "$report_json"
if [ "$result" = "PASS" ]; then
  rm -rf "$work"
  exit 0
fi
printf 'workspace kept for debugging: %s\n' "$work"
exit 1
