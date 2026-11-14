#!/usr/bin/env bash
# The merge queue of one change: test what the land would produce, publish the
# checkpoint, ask the server whether the land may happen, and land exactly the
# commit that was tested.
#
# Runs in the workspace of the change, with the change committed as the pending
# commit (`graft commit -m ...`, not landed). The address, the token and the
# storage come from the environment of that workspace (GRAFT_ADDRESS,
# GRAFT_TOKEN, GRAFT_STORAGE): the land is made as the agent of the change, so
# the server enforces the branch policy on it.
#
# The checkpoint is the trust anchor of the policy, and only the operator may
# move it: the CI holds the shared token too, in GRAFT_OPERATOR_TOKEN.
#
#   BRANCH=main REPO=shop.git GRAFT_OPERATOR_TOKEN=... ci/merge-queue.sh
#
# Exit status: 0 landed, 1 the change is not ready (a test, or the gate), 2 the
# environment is wrong.
set -euo pipefail

branch=${BRANCH:-main}
repo=${REPO:?set REPO to the repository path in the storage, such as shop.git}
work=${WORK:-$(mktemp -d)}
graft=${GRAFT_BIN:-graft}

say() { printf '\n== %s\n' "$*"; }

if [ -z "${GRAFT_ADDRESS:-}" ]; then
  echo "merge-queue: GRAFT_ADDRESS is not set; run inside a graft workspace" >&2
  exit 2
fi

# Only the operator may move the checkpoint. Without its token, the call is
# made as the agent, and the server refuses it (as it should).
operator() {
  if [ -n "${GRAFT_OPERATOR_TOKEN:-}" ]; then
    GRAFT_TOKEN="$GRAFT_OPERATOR_TOKEN" GRAFT_USER="${GRAFT_OPERATOR_USER:-ci}" "$@"
  else
    "$@"
  fi
}

# The committer date is part of the identity of the commit the land writes, so
# one value names the commit the queue tests and the commit it lands.
timestamp=${TIMESTAMP:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
echo "merge-queue: branch $branch, timestamp $timestamp"

say "1/6 the unit suite in the workspace, before anything is asked of the server"
# Cheap and local: an agent runs the same command before it asks for a land.
make test-unit

say "2/6 the commit the land would produce, without landing it"
result=$("$graft" land --dry-run --timestamp "$timestamp")
echo "$result"

say "3/6 the whole tree of that commit, in a workspace of its own"
# No --sparse: the tests see every file of the tree the branch will hold.
"$graft" clone "$GRAFT_ADDRESS" "$repo" "$work/verify" --at "$result"

say "4/6 unit and integration suites on the whole tree of the result"
( cd "$work/verify" && make test )

say "5/6 publish the checkpoint, and ask the gate the question the land asks"
# The checkpoint is published from the workspace that was tested: it holds the
# result as it is, and it certifies the whole tree of it. A workspace that
# held part of the tree would certify a tree the suites never saw, and the
# server would refuse the land.
( cd "$work/verify" && operator "$graft" green set --branch "$branch" --commit "$result" )
if ! "$graft" gate check --branch "$branch" --result "$result"; then
  # The gate refused the land, and its reason says what to do: when the result
  # is stale, the branch moved while the suite ran, and the queue computes the
  # result again on the tip the branch holds.
  echo "merge-queue: the gate refused the land of $result" >&2
  echo "merge-queue: a stale result means the branch moved while the suite ran:" >&2
  echo "merge-queue: the queue runs again on the new tip" >&2
  exit 1
fi

say "6/6 land the commit that was tested, with the timestamp that names it"
"$graft" land --timestamp "$timestamp"
