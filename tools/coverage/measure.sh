#!/usr/bin/env bash
# Measure the statement, block and branch coverage of the graft-owned code.
#
# The scope is the packages graft owns: internal/graft/..., the graft commands,
# the sdk and the graft authentication of the server. The conformance, e2e and
# coretest packages are left out: they drive the code from the outside and
# their own helpers count as the code under test.
#
# Usage: tools/coverage/measure.sh [output-directory]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

out="${1:-${TMPDIR:-/tmp}/git-ai-coverage}"
mkdir -p "$out"

packages="$(go list ./internal/graft/... ./cmd/graft/... ./sdk/... ./internal/gitaly/server/auth 2>/dev/null |
	rg -v '/(conformance|e2e|coretest)$')"

echo "testing ${packages}" | tr '\n' ' ' | cut -c1-200
echo

go test -count=1 -timeout 30m -covermode=atomic -coverprofile "$out/cover.out" $packages

go run ./tools/coverage -profile "$out/cover.out" -root "$root" -prefix 'internal/graft/,sdk/,cmd/graft,internal/gitaly/server/auth' -json |
	tee "$out/packages.txt"

echo
echo "all packages of the scope:"
go run ./tools/coverage -profile "$out/cover.out" -root "$root"
