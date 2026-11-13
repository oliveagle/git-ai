#!/usr/bin/env bash
# promote.sh — replace the production lane with the release the staging lane
# has already passed.
#
#   deploy/promote.sh [version]
#
# The promotion builds nothing and it takes no bundle from this machine: it
# installs the very tarball the staging lane ran, whose checksums, database and
# bucket the integration suite exercised. The gate is checked first
# (host-scripts/gate-check.sh), then the tested bundle is copied into the
# production lane and installed by the same deploy-lane.sh the staging lane
# used, which health-checks the result and rolls back on failure.
#
#   MAX_AGE_HOURS=24   how old the staging report may be
set -euo pipefail

# shellcheck source=deploy/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

version="${1:-$(lane_current_version staging)}"
[ -n "$version" ] || die "no version given and the staging lane reports none"
[ "$version" != "none" ] || die "the staging lane has no release installed"

export STAGING_DIR="$(lane_dir staging)"
export PROD_DIR="$(lane_dir prod)"
export VERSION="$version"
export MAX_AGE_HOURS="${MAX_AGE_HOURS:-24}"
export BACKUP_DIR="${BACKUP_DIR:-$HOME/backups/git-ai}"
export LANE_PG_DB="$PROD_PG_DB"

log "checking the gate for $version"
if ! gate_output="$(ssh_script "$DEPLOY_DIR/host-scripts/gate-check.sh" STAGING_DIR VERSION MAX_AGE_HOURS)"; then
  printf '%s\n' "$gate_output" >&2
  die "the gate did not pass for $version"
fi
printf '%s\n' "$gate_output" | sed 's/^/  /' >&2
gate_report="$(printf '%s\n' "$gate_output" | tail -1)"
tested_sha="$(printf '%s\n' "$gate_output" | sed -n 's/^tested bundle: .*(\([0-9a-f]*\))$/\1/p')"

log "copying the tested bundle into the production lane"
bundle_name="git-ai-release-$version.tar.gz"
copied_sha="$(ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" "
  set -e
  mkdir -p '$PROD_DIR/releases/incoming'
  cp -f '$STAGING_DIR/releases/incoming/$bundle_name' '$PROD_DIR/releases/incoming/$bundle_name'
  sha256sum '$PROD_DIR/releases/incoming/$bundle_name' | cut -d' ' -f1")"
[ -n "$copied_sha" ] || die "the bundle could not be copied"
if [ -n "$tested_sha" ] && [ "${copied_sha:0:16}" != "${tested_sha:0:16}" ]; then
  die "the copied bundle (${copied_sha:0:16}) is not the tested one (${tested_sha:0:16})"
fi
log "tested bundle copied, sha256 ${copied_sha:0:16}"

# The release's migrations run against the live database, so the database is
# dumped first and the promotion cites the dump.
log "backing up the production database"
backup_path="$(ssh_script "$DEPLOY_DIR/host-scripts/backup-db.sh" \
  LANE_PG_DB VERSION BACKUP_DIR PG_CONTAINER PG_IN_CONTAINER_PORT | tail -1)"
log "database dumped to $backup_path"

log "deploying $version to the production lane"
BUNDLE_ALREADY_ON_HOST=1 DEPLOY_VERSION="$version" \
  "$DEPLOY_DIR/deploy-lane.sh" prod "$PROD_DIR/releases/incoming/$bundle_name"

running="$(lane_current_version prod)"
[ "$running" = "$version" ] || die "the production lane runs $running after the promotion, not $version"

ts="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
ssh ${DEPLOY_SSH_OPTS} "${DEPLOY_SSH_USER}@${DEPLOY_HOST}" \
  "mkdir -p '$PROD_DIR/reports' && cat > '$PROD_DIR/reports/promote-$version-$ts.json'" <<JSON
{
  "version": "$version",
  "promoted_at": "$ts",
  "gate_report": "$gate_report",
  "bundle_sha256": "$copied_sha",
  "database_dump": "$backup_path",
  "deployed_by": "deploy/promote.sh"
}
JSON

log "promotion complete: the production lane runs $version"
