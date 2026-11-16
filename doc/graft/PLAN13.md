# PLAN13: the mirror queue (design, 2026-11-17)

## 1. The problem

`graft mirror push` publishes one repository to one remote in one call. It is
the right shape for a fallback that a person runs, or that a cron job runs for
one pair of repository and host. It is not the shape of an operator who keeps
a shop mirrored to GitHub and to Gitee, and who wants the history of what was
published and when.

The manual told the operator to "run the command from a cron job or a small
supervisor". The queue is that supervisor, made a part of the client: the
operator names the mirrors once, and the client keeps publishing them, and
keeps the record of what it published.

The host is not part of the design. There is no `graft gitee push` and no
`graft github push`: the URL of the target is the whole routing decision, so
one queue holds a GitHub mirror and a Gitee mirror and a self-hosted one
alike.

## 2. The shape

A **target** is one destination of the queue:

- the graft repository that is published,
- the Git URL of the mirror, of any host that speaks Git,
- the branch patterns that may reach it,
- the credential it uses, and
- how often it is published, and whether it is published at all.

A **task** is one attempt to publish a target. Every run leaves a task
behind, with the outcome: `ok`, `diverged` or `failed`. The tasks are the
history of the queue: they are what an operator reads when a mirror stopped
moving.

The queue is a queue of targets, and the tasks are the work. The names of the
targets are unique, because a second target with the same name would publish
the same repository twice and the operator would not know which one the worker
ran.

## 3. Where the queue lives

In the client, in one file of the operator: `mirror-queue.json` in the
configuration directory of the user by default, and `--queue-file` (or
`GRAFT_MIRROR_QUEUE`) names another one.

The client is the right home for three reasons:

- the credential of a mirror is in the environment of the machine that
  publishes it, and a queue that runs there reads it there,
- the server already makes the Git connection and holds the credential for the
  length of a call, so the queue does not change what the server does,
- the queue is a property of an operator, not of a repository: one queue
  publishes many repositories, and one repository is published to many hosts.

The file is written as a whole: the new content goes to a file next to it and
is moved over it, so that a reader never sees half a queue and a crash never
leaves a broken one. A change of the queue takes a lock of its own, so that
two processes that manage the queue at the same time do not lose a change; a
lock that is left behind by a process that died is taken over after ten
minutes.

The queue holds no secret. A target names the environment variable that
carries its token, and the value is read when the task runs. The file of the
queue therefore never holds a credential, and the file of a target that a
machine cannot reach is still safe to copy.

## 4. The credential of a target

The convention of the host decides the variable and the user: `GITHUB_TOKEN`
and `x-access-token` for GitHub, `GITEE_TOKEN` and `oauth2` for Gitee,
`GITLAB_TOKEN` and `oauth2` for GitLab, and `oauth2` for a host that has no
convention. `--token-env` and `--username` name them explicitly, and
`--ssh-key` (with `--known-hosts`) is the SSH alternative.

The push and the queue build the credential the same way, so a target of the
queue reaches a host exactly as a push does. A token over `http://` is
refused, both when the target is added and when the task runs, because the
environment of the worker is not the environment of the operator who added
the target.

A host with a convention names the variable that carries its token, and a
mirror whose variable is not set fails with the name of that variable, in the
push and in the task alike. The error of the host would hide what the
operator has to put in the environment of the worker. A host without a
convention needs no credential at all, and its mirror is published without
one.

## 5. The schedule

A target carries an interval. A target with an interval is published again
when the interval has passed since its last run, and a target without one
(`--interval 0`, or `--manual`) is only published when a person names it. The
queue run publishes the targets that are due and exits; the worker publishes
them, waits a tick and looks again.

A target that was added is due right away, so that the queue publishes it
without waiting for the next tick. A run that failed is not a lost run: the
target keeps its schedule, the failure is the task of the history, and the
next run tries again. There is no separate retry counter, because the
schedule is the retry: a mirror that fails every time would be retried every
interval, which is what a person wants from a fallback, and the history says
that it never worked.

## 6. The execution

A task publishes the selected references of its target, exactly as a push
does: the server makes the Git connection, the branch patterns are an
allowlist, the tags are always included, and a remote branch whose history is
not an ancestor of the graft branch is kept and reported. The target decides
whether that branch is overwritten, with `--force`, once, and not per run.

The tasks of a run happen at the same time, at most `--concurrency` of them,
because a queue of ten mirrors should not take ten times as long as one
mirror. The worker runs the due targets of every tick, and a failure of one
target does not stop the others.

The outcome of a run is the exit status of the command: 0 when every task
published, 1 when a task kept a divergent branch or could not publish, and 2
when the command line was wrong. The history of the tasks says the same
thing, in the same words, for an operator that reads the queue later.

## 7. The commands

```shell
graft mirror queue add NAME --repo REPO --url URL --branch PATTERN [--interval D]
graft mirror queue remove NAME
graft mirror queue enable NAME
graft mirror queue disable NAME
graft mirror queue list
graft mirror queue show NAME
graft mirror queue run [--target NAME] [--all]
graft mirror queue worker [--tick D]
graft mirror task list [--target NAME] [--limit N]
graft mirror task show ID
```

The commands of the queue and the commands of the push share their flags, so
that a target is described the same way as the push that publishes it. The
flags of the queue are the flags of `graft mirror`, so `graft mirror --help`
lists them.

## 8. The tests

- `internal/graft/mirrorqueue`: the queue refuses an incomplete target, a
  second target of the same name and a queue of an unknown version; the due
  targets are the ones whose schedule says so; the file is written in one
  piece and is private; a change that fails is not written; the changes of
  two processes are both kept.
- `cmd/graft`: the queue publishes a branch to a real Git remote over the
  file protocol; a remote with unrelated history is kept and the task of the
  run is `diverged`; a target waits for its interval, and a person names it
  to publish it at once; the file of the queue holds the name of the token
  variable and never the token; a task whose token is not in the environment
  says which variable is missing; the worker publishes the targets that are
  due and stops when it is interrupted; the commands of the queue refuse a
  command line that cannot be run as written.

## 9. The limits

- The queue is as reliable as the process that runs it. A worker that is not
  running publishes nothing, and the targets that are due are published when
  it runs again.
- The queue is the operator's, on one machine. Two operators with two queues
  publish the same mirror twice, and the second push finds the mirror up to
  date.
- The task history is a record of what the queue did, not a log: it keeps the
  last two hundred tasks.
- The queue does not configure a webhook or poll a pipeline, and it does not
  import what was committed to the mirror. It publishes, and it records.
