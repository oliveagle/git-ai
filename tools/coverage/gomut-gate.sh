#!/usr/bin/env bash
# Gate the mutation score of the security critical packages.
#
# gomut counts a mutant that cannot be built as INVALID: the mutation broke
# the package, so no test could have killed it. Such a mutant is not a
# weakness of the suite, and the gate leaves it out of the score. The score
# is the killed mutants over the mutants that could be killed at all.
#
# Usage: tools/coverage/gomut-gate.sh [threshold] [package...]
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"

threshold="${1:-90}"
shift || true

packages=("$@")
if [ ${#packages[@]} -eq 0 ]; then
	packages=(
		./internal/graft/agentkey/
		./internal/graft/agentrefs/
		./internal/graft/identity/
		./internal/graft/lifecycle/
	)
fi

out="$(mktemp -d)"
trap 'rm -r -- "$out" 2>/dev/null || true' EXIT

status=0
for package in "${packages[@]}"; do
	name="$(basename "$package")"
	report="$out/$name"
	gomut -all-tests -no-cache -report "$report" -format json "$package" >/dev/null 2>&1 || true
	if [ ! -f "$report/report.json" ]; then
		echo "$name: no report"
		status=1
		continue
	fi
	score="$(python3 - "$report/report.json" "$threshold" <<'PY'
import json, sys

report = json.load(open(sys.argv[1]))
threshold = float(sys.argv[2])
score = report["score"]
killed = score["killed"]
survived = score["survived"]
valid = killed + survived
if valid == 0:
	print("no mutant")
	sys.exit(1)
value = 100.0 * killed / valid
print(f"{value:.1f}% ({killed}/{valid} killed, {score['invalid']} invalid)")
sys.exit(0 if value >= threshold else 1)
PY
)"
	if [ $? -ne 0 ]; then
		echo "$name: $score"
		status=1
	else
		echo "$name: $score"
	fi
done

exit $status
