#!/usr/bin/env bash
# gate-check.sh — may this release replace the production lane?
#
# Runs ON the deploy host, on the evidence the staging lane left:
#
#   STAGING_DIR VERSION MAX_AGE_HOURS
#
# The gate is passed when all of these hold:
#
#   1. an integration report exists for VERSION and says PASS;
#   2. every case in it passed (failed = 0);
#   3. it is at most MAX_AGE_HOURS old — a stale pass proves nothing about the
#      release that would be installed now;
#   4. the staging lane still runs VERSION — the thing that passed is the thing
#      that is running there.
#
# It prints the report's path when the gate passes, so the promotion can cite
# it, and exits non-zero with a reason when it does not.
set -euo pipefail

VERSION="${VERSION:?}"
MAX_AGE_HOURS="${MAX_AGE_HOURS:-24}"

die() { printf 'gate refused: %s\n' "$*" >&2; exit 1; }

report="$(ls -t "$STAGING_DIR"/reports/integration-"$VERSION"-*.json 2>/dev/null | head -1 || true)"
[ -n "$report" ] || die "no integration report for $VERSION in $STAGING_DIR/reports"

result="$(sed -n 's/.*"result": "\([A-Z]*\)".*/\1/p' "$report" | head -1)"
failed="$(sed -n 's/.*"failed": \([0-9]*\).*/\1/p' "$report" | head -1)"
passed="$(sed -n 's/.*"passed": \([0-9]*\).*/\1/p' "$report" | head -1)"
[ "$result" = "PASS" ] || die "the newest report for $VERSION is ${result:-unreadable} ($report)"
[ "${failed:-1}" = "0" ] || die "the report for $VERSION counts $failed failures ($report)"

age_seconds=$(( $(date +%s) - $(stat -c '%Y' "$report") ))
max_seconds=$(( MAX_AGE_HOURS * 3600 ))
[ "$age_seconds" -le "$max_seconds" ] || \
  die "the report for $VERSION is $(( age_seconds / 3600 ))h old, older than ${MAX_AGE_HOURS}h ($report)"

running="$(basename "$(readlink -f "$STAGING_DIR/current" 2>/dev/null || echo none)")"
[ "$running" = "$VERSION" ] || die "the staging lane runs $running, not $VERSION"

bundle="$STAGING_DIR/releases/incoming/git-ai-release-$VERSION.tar.gz"
[ -s "$bundle" ] || die "the tested bundle is no longer on the host ($bundle)"

printf 'gate passed: %s cases passed in %s\n' "${passed:-?}" "$report"
printf 'tested bundle: %s (%s)\n' "$bundle" "$(sha256sum "$bundle" | cut -c1-16)"
printf '%s\n' "$report"
