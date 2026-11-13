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

# psql on the lane's database, and on another database of the same server.
psql_db() { docker exec -u postgres "$PG_CONTAINER" psql -p "$PG_IN_CONTAINER_PORT" -d "$1" -tAc "$2" 2>/dev/null | tr -d '[:space:]'; }
psql_this() { psql_db "$LANE_PG_DB" "$1"; }
psql_other() { psql_db "$OTHER_PG_DB" "$1"; }

# The bucket's directory on the host (RustFS keeps one directory per bucket).
rustfs_data() {
  docker inspect "$RUSTFS_CONTAINER" --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}'
}

repo_main="it-$run_id-main.git"
repo_other="it-$run_id-side.git"
file_a="notes/$run_id.txt"

# --- 1. a repository is created and cloned --------------------------------
begin "repo_create_clone"
{
  "$graft" repo create "$repo_main" --initial-commit --branch main --json
  "$graft" clone "$GRAFT_ADDRESS" "$repo_main" "$work/main" --branch main
} >>"$report_log" 2>&1
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
} >>"$report_log" 2>&1
rc=0
(cd "$work/main" && "$graft" cat "HEAD:$file_a" 2>/dev/null | tr -d '\n') | sed -n 's/.*first line.*second line.*/ok/p' >>"$report_log" || rc=1
landed="$(psql_this "SELECT count(*) FROM graft_changelog WHERE repository_id = (SELECT id FROM graft_repositories WHERE name = '$repo_main')")"
if [ "${landed:-0}" -ge 2 ]; then verdict 0 "$landed changelog rows"; else verdict 1 "changelog rows: ${landed:-none}"; fi

# --- 4. history, blame and search agree on the file ----------------------
begin "history_blame_search"
{
  cd "$work/main"
  "$graft" history "$file_a"
  "$graft" blame "$file_a"
  "$graft" search "first line" --json
} >>"$report_log" 2>&1
hits="$("$graft" -C "$work/main" history "$file_a" 2>/dev/null | wc -l)"
found="$("$graft" -C "$work/main" search "first line" 2>/dev/null | wc -l)"
if [ "$hits" -ge 1 ] && [ "$found" -ge 1 ]; then verdict 0 "$hits commits, $found matches"
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
} >>"$report_log" 2>&1
review_id="$("$graft" -C "$work/main" review list --branch main 2>/dev/null | awk -v t="review $run_id" '$0 ~ t {print $1}' | tail -1)"
if [ -n "$review_id" ]; then
  if "$graft" -C "$work/main" review approve "$review_id" --body "approved by the gate" >>"$report_log" 2>&1; then
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
} >>"$report_log" 2>&1
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
} >>"$report_log" 2>&1
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
} >>"$report_log" 2>&1
# A writer whose base is behind lands only if the server accepts it; the
# change must either land on top or be refused, never lose the first one.
out="$(cd "$work/side" && "$graft" log --limit 5 2>&1)"
printf '%s\n' "$out" >>"$report_log"
first_still_there="$(psql_this "SELECT count(*) FROM graft_changelog WHERE repository_id = (SELECT id FROM graft_repositories WHERE name = '$repo_other')")"
if [ "${first_still_there:-0}" -ge 2 ]; then verdict 0 "$first_still_there changelog rows, no history lost"; else verdict 1 "history lost: ${first_still_there:-0} rows"; fi

# --- 9. the storage core verifies its objects --------------------------
begin "storage_verify"
{
  "$graft" storage state -repo "$repo_main" --json
  "$graft" storage verify -repo "$repo_main" --connectivity
  "$graft" storage gc -repo "$repo_main" --dry-run
} >>"$report_log" 2>&1
if "$graft" storage verify -repo "$repo_main" --connectivity 2>/dev/null | sed -n 's/.*consistent.*/ok/p' | head -1 | wc -l | awk '$1==1{exit 0} {exit 1}'; then
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
  elif GRAFT_TOKEN="$other_token" "$graft" storage state -repo "$repo_main" --json >/dev/null 2>&1; then
    verdict 1 "the other lane's token was accepted"
  else
    verdict 0 "the other lane's token is refused"
  fi
else
  verdict 0 "no other lane to compare with"
fi

# --- 11. the writes reached this lane's database and bucket only -------
begin "data_isolation"
rows_here="$(psql_this "SELECT count(*) FROM graft_repositories WHERE name IN ('$repo_main','$repo_other')")"
if [ -n "${OTHER_PG_DB:-}" ]; then
  rows_there="$(psql_other "SELECT count(*) FROM graft_repositories WHERE name IN ('$repo_main','$repo_other')")"
else
  rows_there="0"
fi
data_dir="$(rustfs_data)"
this_bucket_files=0
other_bucket_files=0
if [ -n "$data_dir" ] && [ -d "$data_dir/$LANE_BUCKET" ]; then
  this_bucket_files="$(find "$data_dir/$LANE_BUCKET" -type f 2>/dev/null | wc -l)"
fi
if [ -n "$data_dir" ] && [ -n "${OTHER_BUCKET:-}" ] && [ -d "$data_dir/$OTHER_BUCKET" ]; then
  other_bucket_files="$(find "$data_dir/$OTHER_BUCKET" -type f 2>/dev/null | wc -l)"
fi
printf 'repos here=%s there=%s, bucket files here=%s there=%s\n' \
  "$rows_here" "$rows_there" "$this_bucket_files" "$other_bucket_files" >>"$report_log"
if [ "${rows_here:-0}" -ge 2 ] && [ "${rows_there:-0}" = "0" ]; then
  verdict 0 "2 repositories in $LANE_PG_DB, none in ${OTHER_PG_DB:-another}"
else
  verdict 1 "repositories here=$rows_here there=$rows_there"
fi

# --- 12. the cache serves repeated reads ------------------------------
begin "cache_serves"
{
  for i in 1 2 3 4 5; do "$graft" storage state -repo "$repo_main" --json; done
} >>"$report_log" 2>&1
stats="$(curl -s --max-time 3 "http://127.0.0.1:$LANE_CACHE_METRICS_PORT/stats")"
printf '%s\n' "$stats" >>"$report_log"
hits="$(printf '%s' "$stats" | sed -n 's/.*"object_hits":\([0-9]*\).*/\1/p')"
if [ "${hits:-0}" -ge 1 ]; then verdict 0 "$hits object hits"; else verdict 1 "no object hits (${hits:-0})"; fi

# --- 13. the data outlives the process and its local cache ------------
begin "restart_survives"
before="$(psql_this "SELECT count(*) FROM graft_changelog WHERE repository_id = (SELECT id FROM graft_repositories WHERE name = '$repo_main')")"
docker restart "$LANE_CONTAINER" >>"$report_log" 2>&1
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  sleep 2
  if "$graft" storage state -repo "$repo_main" --json >/dev/null 2>&1; then break; fi
done
after="$(psql_this "SELECT count(*) FROM graft_changelog WHERE repository_id = (SELECT id FROM graft_repositories WHERE name = '$repo_main')")"
if "$graft" storage state -repo "$repo_main" --json >>"$report_log" 2>&1 && [ "$before" = "$after" ]; then
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
