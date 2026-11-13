#!/usr/bin/env bash
# stage-bundle.sh — put an uploaded bundle where the lane can install it, and
# make its tools available before the release is installed.
#
# Runs ON the deploy host:
#
#   LANE_DIR VERSION BUNDLE      (BUNDLE is the uploaded tarball's path)
#
# The lane needs one of the bundle's tools before it has a release of its own:
# mkbucket creates the lane's bucket. It is extracted to <lane>/.tools, which
# prepare-lane.sh uses and no service reads.
set -euo pipefail

[ -s "$BUNDLE" ] || { echo "no bundle at $BUNDLE" >&2; exit 1; }

mkdir -p "$LANE_DIR/releases/incoming" "$LANE_DIR/.tools"
tar -xzf "$BUNDLE" --strip-components=1 -C "$LANE_DIR/.tools" \
  "git-ai-release-$VERSION/bin/mkbucket"
chmod 755 "$LANE_DIR/.tools/bin/mkbucket"
echo "  bundle staged ($BUNDLE), tools at $LANE_DIR/.tools/bin"
