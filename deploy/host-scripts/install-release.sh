#!/usr/bin/env bash
# install-release.sh — install a release bundle into a lane's releases.
#
# Runs ON the deploy host:
#
#   LANE_DIR VERSION BUNDLE
#
# The bundle is the tarball build-release.sh cut: bin/gitaly, bin/graft,
# bin/graft-cache, bin/mkbucket and SHA256SUMS. It is verified and unpacked
# into releases/<version>/; switch-current.sh makes it the release the lane
# runs, which is a separate step so that a failed install leaves the running
# service alone.
set -euo pipefail

[ -s "$BUNDLE" ] || { echo "no bundle at $BUNDLE" >&2; exit 1; }
mkdir -p "$LANE_DIR/releases"

tmp="$LANE_DIR/releases/.unpack"
rm -rf "$tmp"
mkdir -p "$tmp"
tar -xzf "$BUNDLE" -C "$tmp"
src="$tmp/git-ai-release-$VERSION"
[ -d "$src" ] || { echo "the bundle does not contain git-ai-release-$VERSION" >&2; exit 1; }

(cd "$src" && sha256sum -c --quiet SHA256SUMS) || { echo "checksum mismatch in $BUNDLE" >&2; exit 1; }

target="$LANE_DIR/releases/$VERSION"
if [ -d "$target" ]; then
  echo "  release $VERSION is already installed, keeping the installed copy"
  rm -rf "$tmp"
else
  mv "$src" "$target"
  rm -rf "$tmp"
  echo "  release $VERSION installed in $target"
fi
