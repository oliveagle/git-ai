# Branch management

The branch management policy of a server says what may update a branch and
what a change must carry to land on it. It is the mechanism a CI needs when
git-ai itself does not run the tests: the policy names the branches that are
managed, the reviews a change must carry, and the verification checkpoint that
must cover the branch.

Code: `internal/graft/branchgate` (the decision), `internal/gitaly/hook`
(the enforcement, `enforceBranchGate`), `internal/gitaly/hook/branchgatesource.go`
(the state of a branch: its reviews and its checkpoint),
`internal/gitaly/service/branchpolicy` (the answer to a CI, `proto/branch.proto`),
`cmd/graft/gate.go`
(the CLI that asks the question) and `cmd/graft/green.go` (the CLI that
publishes the checkpoint), and `cmd/graft/commands.go` (the land of a named
commit, `land --commit`). Configuration: `[[graft.branch]]`, see
[CONFIGURATION.md](CONFIGURATION.md).

## The rules

A branch without a rule is **not managed**: it is updated as any other
reference. A managed branch has one rule:

```toml
[[graft.branch]]
name = "main"
require_review = true
require_approvals = 1
allow_stale_base = false
allow_direct_update = false
require_verified = false
```

| Key | Default | What it says |
|---|---|---|
| `name` | required | The branch the rule governs, without `refs/heads/`. |
| `require_review` | `false` | A change that lands on the branch must have a review, approved by the approval rules of `[graft.review]`. |
| `require_approvals` | `0` | The number of current approvals the review must carry, on top of the approvals the approval rules ask for. |
| `allow_stale_base` | `false` | Accept a review whose base is not the current tip of the branch. Without it a change based on an older tip does not land, so that a review always covers what the branch holds. |
| `allow_direct_update` | `false` | Accept an update of the branch that is not a land, such as a push. Without it **only a land may update the branch**. |
| `require_verified` | `false` | The verification checkpoint of the branch must cover it: the checkpoint (`refs/graft/green/<branch>`, moved by [graft-verify](VERIFY.md)) must point at the tip the land builds on, or at the commit the land produces (see [Verifying the result of a land](#verifying-the-result-of-a-land)). |

Two rules for one branch are refused when the configuration is loaded, as are a
rule without a name and a name that is not a branch name.

The rules are read again on SIGUSR1, with the access policy, so a branch can be
put under management, or taken out of it, without a restart: the next update
and the next answer a CI reads are made with the new rules. A file whose rules
the server cannot read (a rule without a name, two rules for one branch) is
rejected as a whole and the rules in force stay, so that a typo never opens a
managed branch. The approval rules of `[graft.review]`, which the gate judges a
review with, still need a restart. See "Reloading the policy" in
[CONFIGURATION.md](CONFIGURATION.md).

## What the server decides

The reference-transaction hook decides every update of a managed branch, in its
prepared phase, while the references are locked and before git writes them: a
refusal aborts the transaction, whatever asked for the update (a push, a land,
a `WriteRef`, an `UpdateReferences`).

The decision of one update:

1. A **deletion** of the branch is refused.
2. A **creation** of the branch by a client is refused. The server creates the
   first branch of a repository with the operator credential.
3. A **direct update** (a push) is refused unless the rule allows it.
4. A **land** must carry the review of every change it lands: a review of the
   commit, on that branch, approved by the approval rules, based on the tip of
   the branch unless the rule allows a stale base. A batched land needs one
   review per change it folds in.
5. When the rule asks for it, the **verification checkpoint** of the branch must
   cover the update: the tip the land builds on, or the commit the land
   produces.

### Who the gate binds

The gate binds the callers that authenticated with a **per-agent token**, like
the access policy of `[[graft.policy]]` does. The operator credential (the
shared token) and the server's own operations are trusted: the verifier may
revert a commit that broke the branch, and the CI may land with the shared
token. See [AGENT-SECURITY.md](AGENT-SECURITY.md) for the trust model.

A CI that lands with the operator credential asks the same question with
`graft gate check`, and refuses the land when the answer is no. The gate is
then the CI's required status check, not the server's.

### The decisions

| Code | What it says |
|---|---|
| `allowed` | The policy allows the update. |
| `not_managed` | The branch has no rule. |
| `direct_update_denied` | The update is not a land of a branch that only a land may update. |
| `deletion_denied` | The branch is managed and cannot be deleted. |
| `creation_denied` | The branch is managed and a client may not create it. |
| `no_branch` | The branch does not exist. |
| `no_review` | The change has no review on the branch. |
| `stale_review` | The review of the change is not based on the tip of the branch. |
| `not_approved` | The review of the change is not approved. |
| `not_enough_approvals` | The review of the change has fewer approvals than the rule asks for. |
| `not_verified` | The verification checkpoint of the branch covers neither its tip nor the commit the update produces. |

## The CI commands

```
graft gate check --branch main [--commit OID] [--direct] [--repo REPO] [--json]
graft gate rules [--repo REPO] [--json]
```

`gate check` asks the server whether a change may land on a branch. The server
answers, with the rules it enforces (`[[graft.branch]]`) and with the state it
decides with: the tip of the branch, its open reviews and its verification
checkpoint. The RPC is `BranchPolicyService.GetBranchDecision`; the hook and
the service decide with one policy (`branchgate.Policy`), so the answer a CI
reads is the answer the server gives when the land arrives, not a second
opinion about it. The CI holds no copy of the policy, and a stale copy cannot
make its status check disagree with the server.

- `--commit OID` checks that change, as a land of it on the branch. Without it
  the pending commit of the workspace is used, and without a workspace only
  the state of the branch is checked.
- `--direct` asks about an update that is not a land, such as a push: it is
  the question of a branch that only a land may update.
- `--json` prints the decision as one JSON object: the branch, the commit, the
  tip, the checkpoint, the number of reviews, and the outcome (`allowed`,
  `code`, `reason`), with the rule the decision was made with.

Exit status: 0 when the policy allows the change, 1 when it refuses it (the
reason is printed), 2 when the command line is wrong.

`gate rules` prints the rules the server enforces, so that the policy a CI is
the status check of can be read without opening the server's configuration
file.

Both commands are accessors of the server, and an agent token may call them:
an agent asks the same question the CI asks. The checkpoint the answer carries
is the commit the branch is green at, not a reference an agent may move: only
an operator moves the checkpoint.

## The checkpoint command

```
graft green set --branch main --commit OID [--repo REPO] [--json]
graft green show --branch main [--repo REPO] [--json]
```

The checkpoint of a branch is the reference `refs/graft/green/<branch>`. It
names the commit whose tree passed the tests. `graft-verify` moves it when it
runs the tests itself; a CI that runs its own tests moves it with
`graft green set`.

- `green set` moves the checkpoint to the commit, with a compare-and-swap: the
  move fails when the checkpoint moved since it was read, so that two CIs
  cannot overwrite each other. The commit must exist in the repository.
- `green show` prints the commit the checkpoint points at and the tip of the
  branch, and tells whether the branch is green.

The command needs the shared token: the checkpoint is the trust anchor of the
branch management policy, and only an operator may move it.

Exit status: 0 when the checkpoint was moved or read, 1 when the move was
refused, 2 when the command line is wrong.

## Landing a commit the CI holds

A CI commonly holds the object ID of the change and the address of the server,
not a checkout of the agent's workspace. `graft land` takes the commit and the
branch itself then, and needs no workspace:

```
graft land main --commit OID [--dry-run] [--timestamp T] [--strategy S] [--repo REPO]
```

- `--commit OID` lands that commit instead of the pending commit of a
  workspace. The commit is resolved against the server first, so one the
  repository does not hold is an error that names it. `BRANCH` is required:
  without a workspace there is nothing to take it from.
- With `-address` and `--repo` the command works anywhere; inside a workspace
  the repository and the address of the workspace are used.
- `--dry-run` and `--timestamp` behave as they do for a pending commit. The
  commit the dry run names is the one the land writes for the same `--commit`,
  `--timestamp`, `--user` and tip, which is what the merge queue below needs.
- It takes neither `--resolve`, `--conflicts-dir` nor `--resolved`: a
  resolution belongs to the workspace that has the conflict package, and a
  named commit is landed as it is or not at all.

## How a CI uses it

A CI that runs its own tests and lands with the shared token does four
things:

1. It takes the whole tree of the branch (`graft clone` without `--sparse`, or
   the full merged tree the server resolves). A sparse checkout hides the
   files the tests touch, and the gate asks about the whole tree.
2. It runs the tests on the tip of the branch.
3. When the tests pass, it publishes the checkpoint:
   `graft green set --branch main --commit <tip>`.
4. It asks the gate whether the change may land:
   `graft gate check --branch main --commit <oid>`.
   The exit status is the status check of the merge request: 0 allows the
   merge, 1 refuses it, 2 says that the CI itself is misconfigured.

The checkpoint covers the tip the land builds on, so a stale checkpoint
refuses the merge instead of letting it through, and the checkpoint of the
new tip is published by the next run of the CI or of `graft-verify`.

That is the post-merge model: land, verify the branch, revert what broke it
(`graft-verify`, [VERIFY.md](VERIFY.md)). It has one gap, a change that is green
alone and red together with the tip, and it paces the branch: after every land
the checkpoint is behind the new tip, so the next land waits for the next run.
The next section closes both.

## Where a project's tests belong

git-ai runs none of a project's tests. It gives the policy three places to
attach them to, and a suite belongs to the one whose question it answers:

| Suite | Where it runs | What it decides |
|---|---|---|
| unit (seconds, no service) | the workspace of the agent before `graft land`; again in the CI, on the result | the change is right; the agent's run is a pre-check, the CI's run is the one the checkpoint covers |
| integration (a database, containers, minutes) | the CI, on the commit the dry run named | the change is right once it is on the branch, which is the tree the branch will hold |
| end to end, slow or flaky | `graft-verify` after the land, or a nightly job | a broken tip is found and reverted, rather than blocking every queue entry |

Three things hold whichever suite runs where:

- A test runs on the *result*, never on the change alone. Two changes that are
  green apart can be red together, and only the commit a dry run names has both
  in its tree.
- A test that sees part of the tree is not evidence about the whole commit. The
  CI takes the whole tree (`graft clone` without `--sparse`), and the verifier
  refuses a `sparse` set unless `test.allow_sparse = true` says the partial tree
  is what its checkpoint means.
- The tests are code from the tree, written by agents. Run them in a container
  with no network and no credentials, as [VERIFY.md](VERIFY.md) and
  [AGENT-SECURITY.md](AGENT-SECURITY.md) describe.

## Verifying the result of a land

A merge queue verifies the *result* of a land before it happens. The gate
accepts a checkpoint that covers the commit the update produces, not only the
tip it builds on, and the server tells the CI what that commit is:

1. A land with `dry_run` (`sdk.LandOptions.DryRun`, `UserLandRequest.dry_run`)
   returns the commit the land would produce, with the objects it wrote readable
   and nothing of the branch changed. Pass the `Timestamp` the land will use,
   and the same `User`: the commit is a rewrite of the change, and its committer
   date is part of its object ID.
2. The CI materializes that commit and runs the verification on it. Its tree is
   the tree the branch will hold after the land, which is where a change that is
   green alone and red on the tip shows up. `graft clone ADDR REPO DIR --at
   <result>` materializes the whole tree of the commit, which no branch points
   at yet; such a workspace has no branch, so it takes one to land from it.
3. It publishes the commit it verified: `graft green set --branch main --commit
   <result>`.
4. It asks the gate with the result: `graft gate check --branch main --commit
   <change> --result <result>` (`result` in
   `BranchPolicyService.GetBranchDecision`). Without `--result` the answer is
   about the tip, and it refuses: a status check made from the tip alone is
   conservative, never wrong.
5. It lands the same request. The land produces exactly the commit the CI
   verified, so the branch is green the moment it is written, and the next
   change of the queue is verified against it.

A CI written as a script runs the same loop with the command line, in the
workspace of the change:

```
T=$(date -u +%Y-%m-%dT%H:%M:%SZ)
M=$(graft land --dry-run --timestamp "$T")   # the commit the land would produce
graft clone "$GRAFT_ADDRESS" repo.git ./verify --at "$M"   # the whole tree of M
(cd ./verify && make test)
graft green set --branch main --commit "$M"
graft gate check --branch main --result "$M"
graft land --timestamp "$T"
```

The same loop without a workspace — the CI holds the object ID of the change:

```
T=$(date -u +%Y-%m-%dT%H:%M:%SZ)
M=$(graft land main --commit "$C" --dry-run --timestamp "$T")
graft clone "$GRAFT_ADDRESS" repo.git ./verify --at "$M"
(cd ./verify && make test)
graft green set --branch main --commit "$M"
graft gate check --branch main --commit "$C" --result "$M"
graft land main --commit "$C" --timestamp "$T"
```

`graft land --dry-run` prints the object ID alone, so the script can take it.
`--timestamp` is required there, and the same date is passed to the land: the
committer date is part of the identity of the commit, and a land with another
one writes another commit, which the checkpoint would not cover.

If the branch moves between the dry run and the land, the land produces another
commit: the checkpoint covers neither it nor the tip, and the land is refused.
That is the intended answer — the queue runs again on the new tip.

A dry run writes the objects of the commit it names into the repository, and no
reference points at them: the regular GC collects them if the land never
comes. It does not resolve conflicts (a result that depends on a resolution is
the resolution), and it is not a land: the branch policy decides updates, and a
dry run makes none.

## What it does not do

- The gate does not run the tests. It asks whether the checkpoint that says the
  branch is green covers the branch, and whether a review of the change is
  approved. Running the tests is the job of the CI, or of
  [graft-verify](VERIFY.md).
- The gate does not bind the operator credential on the server. A CI that
  lands with the shared token must run `graft gate check` itself.
- The gate does not merge. It decides; the land does the work.
