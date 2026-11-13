#!/usr/bin/env bash
# A standard project under Graft, end to end: unit tests in the workspace of the
# agent, unit and integration tests on the commit the land would produce, the
# checkpoint, the gate, the land of the CI, and the post-merge verifier.
#
# Usage: examples/graft/standard-project/run.sh [WORKDIR]
# Requires: a built checkout (make build), Go and make on PATH. Nothing runs
# outside WORKDIR.
#
# What the parts show:
#   1. a change that passes both suites lands, through the merge queue;
#   2. a change that breaks the unit suite is stopped in the workspace, before
#      the server is asked anything;
#   3. a change that passes the unit suite and breaks the integration suite is
#      stopped by the queue, and the agent cannot land it around the queue:
#      main is the ref of the CI;
#   4. an emergency update by the operator leaves the checkpoint behind the tip,
#      so the branch is not green until the verifier has covered it;
#   5. the verifier tests the new tip, and the queue lands the reviewed change.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$here/../../.." && pwd)
work=${1:-$(mktemp -d)}
bin=$work/bin

log() { printf '\n########## %s\n' "$*"; }
note() { printf '*** %s\n' "$*"; }

for b in gitaly gitaly-hooks gitaly-ssh graft graft-verify; do
  if [ ! -x "$repo_root/_build/bin/$b" ]; then
    echo "run.sh: $repo_root/_build/bin/$b is missing; run make build" >&2
    exit 1
  fi
done
command -v go >/dev/null || { echo "run.sh: go is not on PATH" >&2; exit 1; }
command -v make >/dev/null || { echo "run.sh: make is not on PATH" >&2; exit 1; }

mkdir -p "$bin" "$work/run" "$work/repos" "$work/tokens"
for b in gitaly gitaly-hooks gitaly-ssh graft graft-verify; do
  cp "$repo_root/_build/bin/$b" "$bin/"
done
chmod +x "$bin"/*

log "0. the server, the identities and the repository"
for agent in builder reviewer ci; do
  openssl rand -hex 32 > "$work/tokens/$agent"
  chmod 0600 "$work/tokens/$agent"
done
sed "s|@WORK@|$work|g" "$here/ci/config.toml" > "$work/config.toml"
sed "s|@WORK@|$work|g" "$here/ci/graft-verify.toml" > "$work/graft-verify.toml"

"$bin/gitaly" serve "$work/config.toml" >"$work/gitaly.log" 2>&1 &
pid=$!
cleanup() { kill "$pid" 2>/dev/null || true; }
trap cleanup EXIT

for _ in $(seq 300); do
  [ -S "$work/run/gitaly.sock" ] && break
  sleep 0.1
done
if [ ! -S "$work/run/gitaly.sock" ]; then
  echo "run.sh: the server did not come up, see $work/gitaly.log" >&2
  exit 1
fi

export GRAFT_ADDRESS="unix:$work/run/gitaly.sock"
export GRAFT_STORAGE=default
shared=$(sed -n 's/^token = "\(.*\)"$/\1/p' "$work/config.toml")

# as_agent runs a command with the token and the identity of one agent.
as_agent() {
  local agent=$1
  shift
  GRAFT_TOKEN="$(cat "$work/tokens/$agent")" \
    GRAFT_USER="$agent" \
    GRAFT_AUTHOR="$agent <$agent@example.com>" \
    "$@"
}

# as_operator runs a command with the shared token. The user it names is the
# one the access policy sees, so the initial commit and the reads the operator
# makes are made as the CI, the identity that owns main.
as_operator() {
  GRAFT_TOKEN="$shared" \
    GRAFT_USER=ci \
    GRAFT_AUTHOR="ci <ci@example.com>" \
    "$@"
}

# queue runs the merge queue of the workspace as the CI, the only identity the
# access policy lets update main. It holds the shared token too, for the
# checkpoint, which only the operator may move.
queue() {
  local scratch
  scratch=$(mktemp -d "$work/q.XXXXXX")
  (
    cd "$work/shop" || exit 2
    GRAFT_TOKEN="$(cat "$work/tokens/ci")" \
      GRAFT_USER=ci \
      GRAFT_AUTHOR="ci <ci@example.com>" \
      BRANCH=main REPO=shop.git WORK="$scratch" GRAFT_BIN="$bin/graft" \
      GRAFT_OPERATOR_TOKEN="$shared" "$here/ci/merge-queue.sh"
  )
}

as_operator "$bin/graft" repo create shop.git --initial-commit --branch main
as_operator "$bin/graft" clone "$GRAFT_ADDRESS" shop.git "$work/shop" --branch main
cp -a "$here/project/." "$work/shop/"
(
  cd "$work/shop"
  as_operator "$bin/graft" commit -m "the shop project"
  as_operator "$bin/graft" land
)
note "main holds the project; the access policy and the rules on main:"
as_operator "$bin/graft" gate rules --repo shop.git

log "1. a change that passes both suites"
cat > "$work/shop/internal/price/discount.go" <<'GO'
package price

// Discount takes basisPoints off total, rounded down to the cent.
func Discount(total, basisPoints int) int {
	if basisPoints <= 0 {
		return total
	}
	return total - total*basisPoints/10000
}
GO
cat > "$work/shop/internal/price/discount_test.go" <<'GO'
package price

import "testing"

func TestDiscount(t *testing.T) {
	for _, tc := range []struct{ total, bp, want int }{
		{total: 1000, bp: 0, want: 1000},
		{total: 1000, bp: 1000, want: 900},
		{total: 999, bp: 500, want: 950},
	} {
		if got := Discount(tc.total, tc.bp); got != tc.want {
			t.Fatalf("Discount(%d, %d) = %d, want %d", tc.total, tc.bp, got, tc.want)
		}
	}
}
GO
(
  cd "$work/shop"
  note "builder runs the unit suite before it commits anything:"
  make test-unit
  as_agent builder "$bin/graft" commit -m "price: add a discount helper"
  as_agent builder "$bin/graft" review create --branch main --title "price: add a discount helper"
)

log "1a. the queue, before the review is approved"
if queue; then
  echo "run.sh: the queue landed a change nobody approved" >&2
  exit 1
else
  note "the queue refused: the review is not approved yet, so nothing landed"
fi

log "1b. the reviewer approves the change"
(
  cd "$work/shop"
  id=$(as_agent reviewer "$bin/graft" review list --branch main | cut -f1 | sed -n 1p)
  as_agent reviewer "$bin/graft" review approve "$id" --body "the arithmetic is right"
)

log "1c. the queue again: tested on the result, checkpointed, gated, landed"
queue
note "what main holds now:"
as_operator "$bin/graft" green show --branch main --repo shop.git

log "2. a change that breaks the unit suite: stopped in the workspace"
cat > "$work/shop/internal/price/discount.go" <<'GO'
package price

// Discount takes basisPoints off total, rounded down to the cent.
func Discount(total, basisPoints int) int {
	if basisPoints <= 0 {
		return total
	}
	// A negative discount: the unit suite catches it in a second.
	return total + total*basisPoints/10000
}
GO
if ( cd "$work/shop" && make test-unit ); then
  echo "run.sh: the unit suite passed on a broken discount" >&2
  exit 1
else
  note "the unit suite failed in the workspace: no commit, no land, nothing asked of the server"
fi
( cd "$work/shop" && as_agent builder "$bin/graft" revert )

log "3. a change that passes the unit suite and breaks the integration suite"
# The service renames the field of its answer. No unit test reads the wire
# format, so the unit suite is green; the integration suite is not.
sed -i 's/{"cents": sum}/{"total_cents": sum}/' "$work/shop/cmd/shopd/main.go"
(
  cd "$work/shop"
  as_agent builder "$bin/graft" commit -m "shopd: rename the field of the answer"
  as_agent builder "$bin/graft" review create --branch main --title "shopd: rename the field of the answer"
  id=$(as_agent reviewer "$bin/graft" review list --branch main | cut -f1 | sed -n 1p)
  as_agent reviewer "$bin/graft" review approve "$id" --body "fine by me"
)
if queue; then
  echo "run.sh: the queue landed a change the integration suite rejects" >&2
  exit 1
else
  note "the integration suite failed on the tree of the result: no checkpoint, no land"
fi

log "3a. the same change, around the queue"

(
  cd "$work/shop"
  timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  result=$(as_agent builder "$bin/graft" land --dry-run --timestamp "$timestamp")
  note "the gate, asked by the agent about the commit that land would produce:"
  as_agent builder "$bin/graft" gate check --branch main --result "$result" || true
  note "the land itself, as builder:"
  as_agent builder "$bin/graft" land --timestamp "$timestamp" || true
  as_agent builder "$bin/graft" land --abort
  as_agent builder "$bin/graft" revert
)
note "the gate reads the checkpoint, not the exit status of a test run: the"
note "checkpoint covers the tip, and the rule asks for the commit the land"
note "produces, so the answer is not_verified. main is the ref of the CI as well,"
note "so the land is refused before the branch could move on a tip that was green"
note "before this change."

log "3b. an emergency update by the operator leaves the checkpoint behind"
(
  cd "$work/shop"
  printf '\nDeployed by hand on a Friday.\n' >> README.md
  as_operator "$bin/graft" commit -m "docs: a note from the operator"
  as_operator "$bin/graft" land
)
as_operator "$bin/graft" green show --branch main --repo shop.git || true

log "3c. a reviewed change, ready for the queue on the covered tip"
cat > "$work/shop/internal/price/round.go" <<'GO'
package price

// RoundTo returns total rounded to the nearest multiple of step.
func RoundTo(total, step int) int {
	if step <= 0 {
		return total
	}
	rest := total % step
	if rest*2 < step {
		return total - rest
	}
	return total - rest + step
}
GO
cat > "$work/shop/internal/price/round_test.go" <<'GO'
package price

import "testing"

func TestRoundTo(t *testing.T) {
	for _, tc := range []struct{ total, step, want int }{
		{total: 1250, step: 100, want: 1300},
		{total: 1240, step: 100, want: 1200},
		{total: 1250, step: 0, want: 1250},
	} {
		if got := RoundTo(tc.total, tc.step); got != tc.want {
			t.Fatalf("RoundTo(%d, %d) = %d, want %d", tc.total, tc.step, got, tc.want)
		}
	}
}
GO
(
  cd "$work/shop"
  as_agent builder "$bin/graft" commit -m "price: round a total to a step"
  as_agent builder "$bin/graft" review create --branch main --title "price: round a total to a step"
  id=$(as_agent reviewer "$bin/graft" review list --branch main | cut -f1 | sed -n 1p)
  as_agent reviewer "$bin/graft" review approve "$id" --body "the rounding is right"
)

log "4. post-merge: the verifier tests the tip and moves the checkpoint"
note "before:"
as_operator "$bin/graft" green show --branch main --repo shop.git || true
"$bin/graft-verify" -config "$work/graft-verify.toml" -once
note "after:"
as_operator "$bin/graft" green show --branch main --repo shop.git

log "5. the queue lands the reviewed change on the covered tip"
queue
note "what main holds now:"
as_operator "$bin/graft" green show --branch main --repo shop.git

log "done"
note "work directory: $work"
