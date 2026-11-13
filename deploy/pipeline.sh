#!/usr/bin/env bash
# pipeline.sh — the whole road, from a commit to the production lane.
#
#   deploy/pipeline.sh [--promote] [--version VERSION] [--bundle PATH] [--lane staging]
#
#   1. build a release bundle from the current commit (or take --bundle);
#   2. deploy it to the staging lane;
#   3. run the integration suite against staging — the gate;
#   4. only with --promote, and only if the gate passed, replace production
#      with the same bundle.
#
# Without --promote the run stops after the gate, which is the normal way to
# look at a release: the staging lane keeps it, production is untouched, and a
# later `deploy/promote.sh` re-checks the gate and finishes the job.
set -euo pipefail

# shellcheck source=deploy/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

promote=0
version=""
bundle=""
lane="staging"
while [ $# -gt 0 ]; do
  case "$1" in
    --promote) promote=1; shift ;;
    --version) version="${2:-}"; shift 2 ;;
    --bundle) bundle="${2:-}"; shift 2 ;;
    --lane) lane="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,20p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$lane" = "staging" ] || warn "the gate is meant for the staging lane; deploying $lane directly"

if [ -z "$bundle" ]; then
  log "step 1/3: building the release"
  bundle="$(RELEASE_VERSION="$version" "$DEPLOY_DIR/build-release.sh")"
  bundle="$(printf '%s\n' "$bundle" | tail -1)"
else
  log "step 1/3: using the bundle at $bundle"
fi
[ -s "$bundle" ] || die "no bundle at $bundle"
version="$(basename "$bundle")"; version="${version%.tar.gz}"; version="${version#git-ai-release-}"

log "step 2/3: deploying $version to the $lane lane"
"$DEPLOY_DIR/deploy-lane.sh" "$lane" "$bundle"

log "step 3/3: running the integration suite on the $lane lane"
"$DEPLOY_DIR/integration-test.sh" "$lane" "$version"

if [ "$promote" = "1" ]; then
  log "the gate passed; promoting $version to the production lane"
  "$DEPLOY_DIR/promote.sh" "$version"
  log "done: production runs $version"
else
  log "the gate passed on the $lane lane; production is untouched"
  log "promote with: deploy/promote.sh $version"
fi
