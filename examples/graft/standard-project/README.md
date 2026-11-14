# A standard project under Graft

Graft runs none of a project's tests. A project keeps its own suites and tells
Graft two things: the command that runs them (`make test`) and the commit the
command was run on (the checkpoint). This directory is a small project wired up
that way, and `run.sh` runs the whole loop against a real server.

```
project/                   the project
  internal/price/          the unit suite: seconds, no service
  cmd/shopd/               the service
  test/integration/        the integration suite: builds and runs the service
  Makefile                 test-unit, test-integration, test
ci/
  merge-queue.sh           verify the result of a land, checkpoint, gate, land
  config.toml              the server: identities, access policy, branch rule
  graft-verify.toml        the post-merge verifier
run.sh                     the loop, end to end, in a temporary directory
```

## Where each suite runs

| Suite | Runs in | Command | What it decides |
|---|---|---|---|
| unit | the agent's workspace, then again in the CI | `make test-unit` | the change is right |
| integration | the CI, on the commit the land would produce | `make test-integration` | the change is right once it is on the branch |
| end to end, slow or flaky | after the land, or nightly | `graft-verify -once` | a broken tip is found and reverted, not every queue entry blocked |

Three rules hold wherever a suite runs:

- It runs on a *result*, not on a change alone. Two changes that are green apart
  can be red together, and only the commit a dry run names has both in its tree.
- It sees the *whole* tree. `graft clone` without `--sparse`, and `graft-verify`
  refuses a `sparse` set unless `test.allow_sparse = true` says the partial tree
  is what the checkpoint means. The checkpoint says which tree was tested, so a
  branch whose rule asks for the whole tree (`require_full_tree`) refuses the
  land of a change whose tree was not tested whole.
- Its exit status is the contract, and the code it runs is code from the tree:
  run it in a container with no network, no credentials and resource limits
  (see [VERIFY.md](../../../doc/graft/VERIFY.md) and
  [AGENT-SECURITY.md](../../../doc/graft/AGENT-SECURITY.md)).

## The loop

`ci/merge-queue.sh` is the merge queue of one change, run in the workspace of
the change with the change committed as the pending commit:

```
make test-unit                          # 1. cheap, local
graft land --dry-run --timestamp "$T"   # 2. the commit the land would produce
graft clone ADDR REPO DIR --at "$M"     # 3. its whole tree, no branch
make test                               # 4. both suites, on that tree
graft green set --branch main --commit "$M" \
      --suite unit --suite integration                 # 5. the checkpoint
graft gate check --branch main --result "$M"         #    the status check
graft land --timestamp "$T" --expect "$M"   # 6. the land of that exact commit
```

`--timestamp` is required: the committer date is part of the object ID, and a
land with another one writes another commit, which the checkpoint would not
cover. `--expect` holds the land to the commit the suites ran on: the server
refuses to write another one, so a branch that moved while the suites ran
takes nothing, and the queue computes the result again. A CI that holds only
the object ID of the change, and no workspace, runs the same loop with `graft
land main --commit "$C" --dry-run --timestamp "$T"` and `--commit "$C"` on the
gate check and the land
([BRANCHES.md](../../../doc/graft/BRANCHES.md#landing-a-commit-the-ci-holds)).

## What the server enforces

`ci/config.toml` puts main under three rules at once:

- the access policy lets only the `ci` identity write `refs/heads/main`, so a
  change cannot land around the queue even when its review is approved;
- `[[graft.branch]]` refuses every update of main that is not a land
  (`allow_direct_update = false`), so a push is out;
- and refuses a land without an approved review and without a checkpoint that
  covers the commit the land produces (`require_review`,
  `require_verified_result`), not only the tip it builds on;
- and refuses a checkpoint that certifies only part of the tree
  (`require_full_tree`): the suites have to run on the whole tree of the
  result;
- and refuses a checkpoint that certifies only some of the suites
  (`require_suites`): the unit suite alone never carries a land.

`require_verified_result` is what makes the queue the only way in rather than a
strong habit: a checkpoint on the tip says the branch *was* green, and this rule
does not take that for the result of the next land. A land whose result nobody
published is refused with `not_verified`, and a caller that does not name the
result at all with `result_unknown` — so the server, not the CI's discipline,
decides whether the result was tested.

`require_suites` says which suites the result needs. The queue publishes them
with the checkpoint (`graft green set --suite unit --suite integration`), and
the server refuses a land whose checkpoint certifies only some of them, with
`suites_missing`: the unit suite of the workspace is a pre-check of the agent,
not the evidence the branch takes.

The result the queue computed belongs to the tip it computed on. When the
branch moves while the suite runs, the result is `stale_result`: the land would
write another commit, and the checkpoint would cover a commit the branch never
holds. The gate says so before the land (`graft gate check` answers
`stale_result`), so the queue recomputes the result on the tip the branch
holds instead of publishing a checkpoint for a commit nobody will land.

The gate reads the review and the checkpoint, not the exit status of a test run.
A change that is green on its own and red on the tip is the queue's question:
the queue is the only thing that tests the result. When the checkpoint is behind
the tip (an emergency update by the operator) the branch is not green until
`graft-verify` covers the new tip again.

## Running it

```shell
make build                                  # once, from the repository root
examples/graft/standard-project/run.sh      # or: run.sh /tmp/shop
```

It needs Go and make on `PATH`, plus the built `gitaly`, `graft` and
`graft-verify`. Nothing runs outside the work directory. It shows, in order:

1. a change that passes both suites: the queue tests the result, publishes the
   checkpoint, asks the gate and lands the commit it tested;
2. a change that breaks the unit suite, stopped in the workspace before the
   server is asked anything;
3. a change that passes the unit suite and breaks the integration suite
   (a renamed field of the HTTP answer), stopped by the queue at step 4, and the
   gate refusing a land of it around the queue (`not_verified`: the checkpoint
   covers the tip, not the commit the land produces) before the access policy
   refuses the land itself;
4. an emergency update by the operator that leaves the checkpoint behind the
   tip: the branch answers "not green" until step 5 covers it;
5. `graft-verify -once` testing the new tip, moving the checkpoint, and the
   queue landing the reviewed change on the covered tip.

## Adapting it to your project

- Replace `make test` in `ci/merge-queue.sh` and `[test] command` in
  `ci/graft-verify.toml` with the command that runs your suites. Keep the exit
  status: 0 green, anything else red.
- Keep `make test-unit` fast enough to run in the agent's workspace; the CI runs
  the whole suite on the tree of the result.
- Bake the toolchains, the database and the service images into the CI runner.
  The loop clones a tree per queue entry, so a runner that installs a toolchain
  per entry pays for it on every entry; `graft-cache` in front of the server
  removes the object transfer, not the test time.
- Long suites: batch the lands (`LAND-BATCHING.md`) so one test run covers
  several changes, and give `graft-verify` a `batch_window` that covers the
  lands of one window.
