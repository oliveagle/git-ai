# PLAN14: the mirror policy (design, 2026-11-17)

## 1. The problem

PLAN13 made the client keep publishing the mirrors of a repository. It did
not make anybody read the result. An operator who decides that a repository
must be mirrored — GitHub, Gitee, GitLab, Aliyun, or a self-hosted instance —
has no lever on the server: a repository whose queue was never added takes
pushes forever, and a mirror that stopped moving is discovered when someone
looks at it.

The requirement is a policy of the server:

- a repository that no mirror publishes takes no push,
- a mirror that falls behind warns from a number of commits,
- a mirror that falls further behind refuses the push,
- and a refusal tells the pusher what to repair.

The warning is the step before the refusal, not a decoration: a policy that
only refuses teaches nobody, and a policy that only warns is a log.
`deny_lag` is refused when it does not follow `warn_lag`, so a server cannot
be configured to refuse a push it never warned about.

## 2. The shape

The state is the server's, because the gate is the server's. A client's queue
is one operator's schedule; the mirror of a repository is a fact about the
repository.

Three pieces:

- **`mirrorstate`**: the targets of a repository, and the state of their last
  publication. `Target{Name, URL, Branches, Enabled}` and
  `Sync{Target, Status, Branches map[branch]commit, PublishedAt, Error}`. A
  store is the storage core's database (`graft_mirror_targets`,
  `graft_mirror_syncs`, one migration) or, when the storage core is off, JSON
  files under `<git dir>/graft/mirror/` written atomically under a lock.
- **`mirrorgate`**: the decision, as pure Go. Policy, the targets with what
  they published and how far behind they are, the repository and the branch in;
  `Decision{Allowed, Code, Reason, Warning}` out. Nothing else reads git or a
  database, so the decision is tested as a table.
- **`MirrorService`**: `RegisterTarget`, `UnregisterTarget`, `ListTargets`,
  plus the policy the server decides with so that a client can explain it.

The publication is recorded where the push happens:
`UpdateRemoteMirror` takes an optional `target` (field 8) and records, after
the push, the commit every published branch reached, whether a divergent ref
was kept, and the error of a failed push. `graft mirror push
--mirror-target NAME` sends it, and every `queue run` sends the target of the
task. A publication that names no target records nothing: a one-off push says
nothing about a mirror of the repository.

## 3. The decision

```
no target at all                      required: no_mirror_target      (refused)
no mirror ever published successfully  required: mirror_never_synced   (refused)
a mirror holds history we do not have  required: mirror_diverged       (refused)
                                      otherwise: warning
lag >= deny_lag > 0                    mirror_lagging                  (refused)
lag >= warn_lag > 0                    mirror_lagging                  (warning)
branch no target publishes             branch_not_mirrored             (warning)
a target never published               mirror_not_synced               (warning)
otherwise                              allowed
```

- The strictest mirror that covers the branch decides: a mirror that is behind
  holds the push back whatever the others hold. A repository with a GitHub and
  a Gitee mirror is as published as its worst mirror.
- The lag is `git rev-list --count TIP ^PUBLISHED...`: the commits the push
  adds that the mirror does not hold, counted against every mirror that
  covers the branch. A mirror that published several branches of the push
  counts with all of them, so a commit that reached the mirror on another
  branch is not counted twice.
- A commit the repository does not have is a divergence, not a lag: the
  mirror was pushed elsewhere, or the branch was rewritten. Lag cannot compare
  it, and a forced publication is a decision of a person.
- A branch no target publishes does not refuse the push. A repository
  publishes its mainline and still takes the branches a person works on; the
  pusher is told, and the warning names the command that adds the branch to a
  target.

## 4. Where it runs

In the reference-transaction hook, in the `prepared` phase: git holds the
locks of the references and has not written them, whatever asked for the
update. It runs after the branch policy and before the storage core commits
the transaction, so a refusal aborts the transaction and leaves no trace in
the database.

The policy is off unless configured. The gate fails closed: a server that sets
`[graft.mirror]` and has no source of the state refuses every push rather than
taking every push — the alternative would turn the lock off where it is
supposed to be.

The warning reaches the pusher: git gives the reference-transaction hook no
side band, so the hook service settles a `hook.TransactionMessages` per call
and writes what it holds to the stderr of the response, then answers exit 0. On
SSH the pusher reads it as `remote:` lines, which is where a refusal is read
too. It is logged as well, with the lag of every mirror as a field.

## 5. The client

- `graft mirror push --mirror-target NAME` registers the target with the
  server when it is not known yet, and records the publication under it.
- `graft mirror queue add` registers the target with the server as it adds it,
  and rolls the queue back when the server refuses it: a target the server does
  not know would count for the worker and for nothing else.
  `queue remove` unregisters it, tolerating a server that never knew it.
- `graft mirror queue register NAME` registers a target the queue already
  holds, which is the repair when the server was unreachable at add time, or
  when the state of the server was lost.
- `graft mirror status --repo REPO [--branch BRANCH] [--json]` asks the
  question the hook decides, with the same targets and the same policy, prints
  every mirror with what it holds and how far behind it is, and exits 1 when a
  push would be refused. It is the view an operator repairs from, and the check
  a schedule or an agent runs before it pushes.

`MirrorService` is not on the agent allowlist: an agent may not declare or
remove the mirrors it is held to account by.

## 6. Rollout

The policy is a lock, so it is turned on in two steps:

1. `required = true` with the default `warn_lag = 10` and no `deny_lag`. Every
   repository must declare a mirror and publish it once; every mirror that is
   behind warns, and nothing is refused.
2. Once every mirror is published by a worker, set `deny_lag` above
   `warn_lag`, for example `deny_lag = 50`. A mirror that the worker cannot
   reach now holds the pushes of its repository, which is the intent: the
   pusher reads which target is behind and which command publishes it.

`graft mirror status` on every repository answers which step is complete
before the server is restarted: a repository that reports `no_mirror_target`
or `mirror_never_synced` is not ready for `required = true`, and the report
says which command repairs it.

## 7. Limits

- The policy is a push policy. It does not stop a fetch or a read, and it does
  not publish anything by itself: a repository that takes no push until its
  mirror is published still needs the worker that publishes it.
- The lag is what the server recorded. A publication that lied about its
  result — a push that reported success without writing the branch — is a
  success the policy cannot see. The reflog of the mirror is the authority.
- The state is per repository, not per branch rule: a mirror that covers
  `main` does not hold the pushes of `release/1.0`, it warns about them.
- `deny_lag` counts commits, not time. A branch that moves once a week and a
  branch that moves once a minute are held to the same number: the operator
  chooses the number for the branch that moves most.
- A refusal is not a notification. The pusher reads it, and the log holds it;
  a schedule that watches the mirror itself sees the same fact later.
