# Mirror to another Git host

Graft is the fast path: agents commit to it, reviews and lands are decided by
its branch policy, and its checkpoint says which result was tested. A mirror is
the compatibility path: the landed branches are published again to a normal Git
server — Gitee, GitHub, GitLab, Gitea, or a self-hosted instance — so the
pipelines, webhooks and people that already work there keep working.

There is one command for every host, and no per-provider subcommand: there is
no `graft gitee push`, `graft github push` or `graft gitlab push`. Graft does
not know or care which product serves the URL, and it does not call any
provider's API: it pushes Git references. A push to the mirror is what starts
the traditional CI.

## The command

```shell
export GRAFT_ADDRESS="unix:/run/graft.sock"
export GRAFT_TOKEN="$SHARED_TOKEN"

graft mirror push --repo shop.git \
  --url https://git.example.com/group/shop.git \
  --branch main --branch 'release/*'
```

The URL is the whole routing decision. The host of `--url` only picks a default
token variable and HTTP Basic user, so a well-known host needs no credential
flags; the table below is a convenience, not the list of supported products.
`--token-env` and `--username` name the credential explicitly for any other
host.

The same command against the three well-known hosts:

```shell
GITEE_TOKEN="$GITEE_ACCESS_TOKEN" graft mirror push --repo shop.git \
  --url https://gitee.com/group/shop.git --branch main

GITHUB_TOKEN="$GITHUB_TOKEN" graft mirror push --repo shop.git \
  --url https://github.com/group/shop.git --branch main

GITLAB_TOKEN="$GITLAB_ACCESS_TOKEN" graft mirror push --repo shop.git \
  --url https://gitlab.com/group/shop.git --branch main
```

| Host | Default variable | Default user |
|---|---|---|
| `gitee.com` | `GITEE_TOKEN` | `oauth2` |
| `github.com` | `GITHUB_TOKEN` | `x-access-token` |
| `gitlab.com` | `GITLAB_TOKEN` | `oauth2` |
| anything else | none | `oauth2` |

A mirror of a host without a convention needs no credential at all. A
well-known host names the variable that carries its token, and a push whose
variable is not set fails with the name of that variable. `--ssh-key` (usually
with `--known-hosts`) is the SSH alternative for any host. A token over
`http://` is refused unless `--allow-insecure-http` is explicit.

`--ssh-key` and `--known-hosts` name **files**, not key material: the client
reads them when it runs, and the contents travel in the request, where the
server writes them into a private file for the length of the Git connection.
A target of the queue stores the two paths and never the secret, so the queue
file can be read by anyone who can read the checkout, and the read happens
again on every run. A path that does not exist, or a file that is empty, is
refused before the Git connection is made, with the path in the message.

The server makes the Git connection and holds the credential for the length of
the call. The CLI needs the shared (operator) token, and the remote token is
read from the environment, so it never appears in `argv` or in the output.

`--branch` is an allowlist and may be repeated; its patterns contain no
`refs/heads/` prefix and may use `*`. Tags are always included, because they
carry commits that branches may name. A backup that publishes more branches
than its operator named is a leak, not a convenience. Nothing under
`refs/graft/` is published: the mirror carries history, not Graft's review or
verification state.

## A reliable fallback loop

`graft mirror push` is a reconciliation, not an event. It compares the selected
references again on every run, batches the updates, and succeeds when the
selected tips are on the mirror. That makes the retry safe:

```shell
graft land ... || exit 1
graft mirror push --repo shop.git --url ... --branch main || true
```

A push that fails after a successful land does not undo the land. The next
scheduled run publishes what the failed run missed. Run the command from a cron
job or a small supervisor as well as after lands; the schedule is the recovery
point objective. A backup that is only pushed once, by hand, is not a fallback.

The mirror is downstream. If a traditional workflow commits to the mirror, the
next run does not import that history silently. Import the change into Graft on
purpose, review it there, and let the next push replace the mirror. If the
mirror becomes the source of truth, that is a different system and this
one-way mirror must be turned off first.

## The mirror queue

One push publishes one repository to one remote. An operator with several
repositories and several hosts — a GitHub mirror and a Gitee mirror of the
same shop — needs more than that, so the client keeps a queue of mirrors:

```shell
graft mirror queue add github-shop --repo shop.git \
  --url https://github.com/group/shop.git --branch main --interval 5m

graft mirror queue add gitee-shop --repo shop.git \
  --url https://gitee.com/group/shop.git --branch main --interval 5m

graft mirror queue list
graft mirror queue run
```

A **target** of the queue is one destination: the graft repository, the Git
URL it is published to, the branches that may reach it, the credential it
uses, and how often it is published. A **task** is one attempt to publish a
target. The queue holds both, and every run leaves a task behind, so an
operator can see what the queue published, when, and what happened.

The queue is a file of the operator, by default `mirror-queue.json` in the
configuration directory of the user, and `--queue-file` names another one. It
holds no secret: a target names the environment variable that carries its
token — `GITHUB_TOKEN` for a GitHub mirror, `GITEE_TOKEN` for a Gitee mirror,
by the convention of the host — and the value of that variable is read when
the task runs. A target of a host without a convention needs no credential at
all and publishes to a public mirror. A target whose variable is not set
fails with the name of the variable, not with a secret, so the task of the
history says what the operator has to put in the environment of the worker.

The commands of the queue:

| Command | What it does |
|---|---|
| `graft mirror queue add NAME` | puts a target into the queue |
| `graft mirror queue list` | lists the targets and their last outcome |
| `graft mirror queue update NAME` | restates a target the queue holds: the same flags as `add`, applied where the target is, keeping its history |
| `graft mirror queue show NAME` | shows one target |
| `graft mirror queue enable NAME` | lets the worker publish the target again |
| `graft mirror queue disable NAME` | leaves the target alone, keeping its history |
| `graft mirror queue remove NAME` | takes the target out of the queue, and out of the mirror policy of the server |
| `graft mirror queue register NAME` | registers a target the queue already holds with the server, without publishing it |
| `graft mirror queue run` | publishes the targets that are due, and exits |
| `graft mirror queue worker` | publishes the targets that are due, over and over |
| `graft mirror task list` | lists the tasks, newest first |
| `graft mirror task show ID` | shows one task |

The declaration of a target changes: a schedule that is too slow for the lag
the policy allows, a branch a release added, a URL that moved. `queue update`
takes the same flags as `add` and applies them where the target is, so the
target keeps the outcome of its last publication — the mirror policy reads that
outcome, and adding the same mirror under a new name would make the repository
look unpublished until the next run. The flags that are left out are the
defaults of a new target, not the values the target had, so an update restates
the declaration rather than editing one field of it.

The interval is a policy input, not only a convenience: `warn_lag` counts
commits, and a mirror published once an hour is an hour behind whatever the
repository accepted meanwhile. Publish a mirror often enough that ordinary work
does not reach the lag the policy warns about, or the warning — and from
`deny_lag`, the refusal — becomes the normal answer to a push.

`queue run` is the run of a cron job: the queue decides what is due, and the
cron job decides how often the queue is looked at. `queue worker` is the
same thing as a process: it publishes what is due, waits `--tick` and looks
again, until it is interrupted.

The schedule of a target is its `--interval`. A target with an interval is
published again when the interval has passed since the last run, and a target
without one (`--interval 0`, or `--manual`) is only published when a person
names it with `--target NAME`. A failed run is not a lost run: the target
keeps its schedule, the failure is the task of the history, and the next run
tries again.

The safety of the push is the safety of the queue. A target that publishes to
a remote branch with unrelated history keeps that branch, the task of the run
is `diverged`, and the run exits 1. Only a target that was given `--force`
overwrites it, and that is a decision of a person, made once, on the target
and not on the run.

## The mirror policy

A queue that nobody watches does not make a mirror. The server can hold the
pushes of a repository to the mirrors that repository declares, so that the
mirror of a repository is as much a part of accepting a push as its branch is:

```toml
[graft.mirror]
# A repository that no mirror target publishes, and whose mirrors never
# published successfully, takes no push.
required = true
# A mirror this many commits behind the branch warns the pusher (default 10).
warn_lag = 10
# A mirror this many commits behind refuses the push. Zero, the default, never
# refuses: a server warns first, and the operator turns the refusal on once a
# worker publishes every mirror on a schedule.
deny_lag = 50
```

The policy is off unless configured: a server that sets none of the three keys
takes every push. `deny_lag` must be greater than `warn_lag`, and a server
whose configuration refuses that at load time does not start.

A mirror target is any Git remote, and neither the policy nor the queue knows
or cares who serves it: a GitHub, Gitee or GitLab repository, a Gitea or Codeup
instance, or a bare repository on a second machine all count, and a target
whose URL is a `file://` path counts too. What `required` asks is that the
repository is published somewhere else and that the publication succeeded at
least once — the copy, not the product. A remote that shares a disk or a
machine with the server is a copy without the point of one.

The policy decides on the state of the publications, not on the queue of any
client. The server records, for every target and every branch, the commit the
mirror holds: `graft mirror push --mirror-target NAME` records it, and so does
every `queue run` of a target. A target that the server does not know counts
for nothing, so `graft mirror queue add` registers the target with the server
as it adds it, and `graft mirror queue register NAME` registers a target that
the queue already holds — the repair when the server was unreachable when the
target was added, or when the server's state was lost.

The gate runs in the `prepared` phase of the reference transaction, while git
holds the locks of the references and before it writes them, so it holds for
every update of a branch, whatever asked for it. The branch patterns of the
targets decide which branches are covered: a push to a branch that no target
publishes is a warning, not a refusal, because a repository publishes its
mainline and still takes the branches a person works on.

Being behind is a property of a branch, not of the repository: the policy
compares the branch a push writes with the commit the mirror published for that
very branch. A branch the mirror never published carries no lag — the next
publication takes the whole branch — so a branch cut from a lagging mainline,
and the temporary branch of a commit that is not landed yet, still reach the
server while the mainline itself takes no push. The commits of the other
branches of the mirror are not compared: if they were, every new branch would
inherit the lag of the branch it was cut from, and a repository whose mirror is
behind would take no commit at all, which is the one way to repair it.

### What the policy answers

| Code | Push | Meaning |
|---|---|---|
| `allowed` | yes | every mirror that covers the branch is up to date |
| `no_mirror_target` | no (with `required`) | the repository declares no mirror target |
| `mirror_never_synced` | no (with `required`) | no mirror of the repository ever published successfully |
| `mirror_diverged` | no (with `required`, else a warning) | a mirror holds the branch at a commit the repository does not have |
| `mirror_lagging` | yes with a warning, no from `deny_lag` | a mirror is behind the branch |
| `branch_not_mirrored` | yes, with a warning | no target publishes this branch |

The strictest mirror that covers the branch decides: one mirror that is behind
holds the push back, whatever the others hold.

What a push reads is the sentence of the decision, on its stderr, next to the
`remote:` lines of the server: the hook exits 1 and git aborts the update, so
the reason and the command that repairs it are in front of the pusher rather
than in a log.

### `graft mirror status`

The command asks the question the hook decides, with the same targets and the
same policy, so it explains a refusal and a warning before they happen:

```shell
graft mirror status --repo shop.git --branch main
graft mirror status --repo shop.git --branch main --json
```

It prints every target with the commit it holds, how far behind it is, the
policy, and the decision — and it exits 0 when a push would go through and 1
when it would not, so a schedule or an agent reads the same answer the push
gets. The `--json` form is the same object with the fields of a machine:

```json
{
  "repository": "shop.git",
  "branch": "main",
  "policy": {"required": true, "warn_lag": 10, "deny_lag": 50},
  "targets": [
    {"name": "github-shop", "url": "https://github.com/group/shop.git",
     "branches": ["main"], "enabled": true, "status": "ok",
     "published": {"main": "0d1e..."}, "lag": 12}
  ],
  "allowed": true,
  "code": "mirror_lagging",
  "warning": "the mirror github-shop of shop.git is 12 commits behind the branch main (a push is refused from 50 commits behind): publish it with \"graft mirror queue run\""
}
```

### Repairing a refusal

The refusal names the command that repairs it, and the warning is written to
the stderr of the push, where git shows it to the pusher as `remote:` lines:

| Code | What the operator does |
|---|---|
| `no_mirror_target` | `graft mirror queue add NAME --repo REPO --url URL --branch PATTERN`, or `graft mirror queue register NAME` when the queue already holds the target |
| `mirror_never_synced` | `graft mirror queue run`, and check the token of the target in the environment of the worker |
| `mirror_lagging` | `graft mirror queue run`; if the mirror stays behind, the worker is not running, or its schedule is longer than the lag the policy allows |
| `mirror_diverged` | someone committed to the mirror, or the branch was rewritten: import the work or decide that Graft is authoritative, then publish the target again with `--force` |
| `branch_not_mirrored` | add the branch to a target with `--branch PATTERN` if it must be mirrored |

A push is also the moment a person reads the answer: a warning says which
mirror is behind and which command publishes it, before the policy refuses
anything.

The commands of this table are the operator's: `MirrorService` is not on the
agent allowlist, because an agent that could remove a target would open the
repository the policy holds, so an agent that reads a refusal reports it with
the reason, which names the target and the command. `graft mirror status` is
the read of the same state, and it needs the shared token as well.

### Turning the policy on for a server

A server holds many repositories, and the policy is per repository, so the
audit before `required = true` is one line per repository of the storage:

```shell
find /var/lib/graft/repos -maxdepth 4 -name '*.git' -type d | while read -r repo; do
  rel=${repo#/var/lib/graft/repos/}
  graft mirror status --repo "$rel" --json |
    jq -r '"\(.code) \(.repository)"' || true
done | sort | uniq -c
```

`allowed` is a repository the policy will let alone, and every other code is a
repair this manual names.

The order matters: the policy is a lock, and a lock that is turned on before
the mirrors exist stops the work it is meant to protect. On a server, in four
steps:

```shell
# 1. The server runs the mirror policy as a warning first (config.toml).
#    [graft.mirror]
#    required = true
#    warn_lag = 10

# 2. For every repository, declare its mirror and publish it once. The
#    command registers the target with the server as it adds it.
graft mirror queue add codeup --repo shop.git \
  --url https://codeup.aliyun.com/group/shop.git --branch 'main' --branch 'release/*' \
  --interval 5m
graft mirror queue run --target codeup

# 3. Read the answer the push will get, before a person meets it.
graft mirror status --repo shop.git --branch main
#   It prints the code of the decision (allowed, mirror_lagging, ...) and
#   exits 1 when a push would be refused.

# 4. Keep the worker running, or the mirrors fall behind and — from deny_lag —
#    the pushes stop. A systemd timer runs `graft mirror queue run` on the
#    schedule of the targets; `graft mirror queue worker` is the same thing as
#    a process.
```

A repository that reports `no_mirror_target` is not ready for `required = true`,
and one that reports `mirror_never_synced` is not ready for `deny_lag`. Repair
those first; the status names the command.

Once every repository in scope is `allowed`, the second step of the policy:

```toml
[graft.mirror]
required = true
warn_lag = 10
deny_lag = 50   # a mirror 50 commits behind holds the pushes of its repository
```

Restart the server, and push once with a mirror that is behind to read the
warning where the pusher reads a refusal — the stderr of the push.

A mirror is a copy of the repository, so it follows it: a branch the target
published and that the repository no longer has is deleted from the mirror by
the next publication. What tells the two apart is the state recorded for the
target: a branch this server published is removed when it goes, while a branch
that appeared on the mirror without the repository having it is somebody's
work and is kept — the boundary below applies to it, and only `--force`
crosses it.

The branches of a workspace are not published at all. `refs/heads/graft/<agent>/`
names a change that has not landed, so even a target that publishes `*`
publishes the branches a person can name, not the drafts of a workspace.

## Divergence and force

By default, `graft mirror push` keeps a remote branch whose history is not an
ancestor of the Graft branch, lists it, writes its result even with `--json`,
and exits 1. That is the reliability boundary: a retry must not erase a
person's work because a token expired, a mirror was reset, or someone committed
there.

Inspect the divergent branch first. If it is only an old backup, run again with
`--force` to make the Graft branch authoritative:

```shell
graft mirror push --repo shop.git --url ... --branch main --force
```

If it holds work, import that work into Graft before forcing. The exit status
is what automation should key on: 0 means the selected references are on the
mirror, 1 means a divergent ref was kept, and 2 means the command line was
wrong.

## Limits

- The mirror carries branches and tags, not Graft's internal `refs/graft/`
  checkpoint and review references. A traditional CI sees history; it does not
  see Graft's approval or verification state.
- The push is as reliable as its schedule. Until a successful push, a failure
  of the Graft storage loses commits that the mirror never saw.
- The command does not configure a webhook or poll a pipeline. Use the mirror's
  own push hook, or poll the traditional CI, for the result. Graft's
  `require_verified_result` remains the branch policy: a green job at the mirror
  does not let a change onto a managed Graft branch unless its checkpoint
  covers the result.
