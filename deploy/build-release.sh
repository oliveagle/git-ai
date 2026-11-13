#!/usr/bin/env bash
# build-release.sh — build the server, the clients and the tools, and cut a
# release bundle.
#
#   deploy/build-release.sh [version]
#
# The bundle is what the lanes install:
#
#   git-ai-release-<version>/
#     VERSION                      the release's name
#     RELEASE                      the commit, the date and the Git builds
#     bin/gitaly bin/graft bin/graft-cache bin/mkbucket
#     SHA256SUMS
#
# The server bundles its own Git (three builds: master, prev and mvcc), so it
# is compiled on this machine and runs on the NAS's ubuntu:24.04 container:
# the bundled Git is linked against the glibc of this machine, 2.39, which the
# container has. A build from an older distribution will not run there.
#
# Meson and Ninja build the bundled Git. GitLab is unreachable from this
# network, so the Git sources come from the GitHub mirror; the mvcc branch
# exists only on GitLab and is replaced by the default build (Git's MVCC
# reference backend is not used by these lanes).
set -euo pipefail

# shellcheck source=deploy/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

repo_root="$(cd "$DEPLOY_DIR/.." && pwd)"
version="$(release_version "$repo_root" "${RELEASE_VERSION:-${1:-}}")"
out_dir="${OUT_DIR:-$repo_root/dist}"
bundle_name="git-ai-release-$version"
bundle_dir="$out_dir/$bundle_name"

git_repo_url="${GIT_REPO_URL:-https://github.com/git/git.git}"
git_prev="$(sed -n 's/^GIT_VERSION_PREV ?= //p' "$repo_root/Makefile")"
git_mvcc="${GIT_VERSION_MVCC:-$git_prev}"
[ -n "$git_prev" ] || die "cannot read GIT_VERSION_PREV from the Makefile"

for tool in meson ninja make go; do
  command -v "$tool" >/dev/null || die "$tool is missing (bundled Git and the Go build need it)"
done

log "building release $version from $(git -C "$repo_root" rev-parse --short HEAD)"
make -C "$repo_root" \
  GOTOOLCHAIN="${GOTOOLCHAIN:-go1.26.0}" \
  GIT_REPO_URL="$git_repo_url" \
  GIT_VERSION_MVCC="$git_mvcc" \
  build

log "building the lane tool"
(cd "$repo_root" && go build -trimpath -o _build/bin/mkbucket ./deploy/tools/mkbucket)

log "assembling $bundle_dir"
rm -rf "$bundle_dir"
mkdir -p "$bundle_dir/bin"
for binary in gitaly graft graft-cache mkbucket; do
  [ -x "$repo_root/_build/bin/$binary" ] || die "_build/bin/$binary was not built"
  install -m 755 "$repo_root/_build/bin/$binary" "$bundle_dir/bin/$binary"
done

printf '%s\n' "$version" >"$bundle_dir/VERSION"
{
  printf 'version=%s\n' "$version"
  printf 'commit=%s\n' "$(git -C "$repo_root" rev-parse HEAD)"
  printf 'built_at=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  printf 'git_prev=%s\n' "$git_prev"
  printf 'git_mvcc=%s\n' "$git_mvcc"
  printf 'gitaly=%s\n' "$("$bundle_dir/bin/gitaly" --version | sed -n 's/^Gitaly, version //p')"
} >"$bundle_dir/RELEASE"

(cd "$bundle_dir" && sha256sum bin/* >SHA256SUMS)
tar -czf "$out_dir/$bundle_name.tar.gz" -C "$out_dir" "$bundle_name"

log "bundle ready: $out_dir/$bundle_name.tar.gz"
sed -n 'p' "$bundle_dir/RELEASE" | sed 's/^/  /' >&2
printf '%s\n' "$out_dir/$bundle_name.tar.gz"
