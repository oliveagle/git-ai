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

A public mirror needs no credential at all. `--ssh-key` (usually with
`--known-hosts`) is the SSH alternative for any host. A token over `http://` is
refused unless `--allow-insecure-http` is explicit.

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
the task runs. A queue on a machine without a token in its environment
publishes to a public mirror, and a queue whose token is missing fails with
the name of the variable, not with a secret.

The commands of the queue:

| Command | What it does |
|---|---|
| `graft mirror queue add NAME` | puts a target into the queue |
| `graft mirror queue list` | lists the targets and their last outcome |
| `graft mirror queue show NAME` | shows one target |
| `graft mirror queue enable NAME` | lets the worker publish the target again |
| `graft mirror queue disable NAME` | leaves the target alone, keeping its history |
| `graft mirror queue remove NAME` | takes the target out of the queue |
| `graft mirror queue run` | publishes the targets that are due, and exits |
| `graft mirror queue worker` | publishes the targets that are due, over and over |
| `graft mirror task list` | lists the tasks, newest first |
| `graft mirror task show ID` | shows one task |

`queue run` is the run of a cron job: the queue decides what is due, and the
cron job decides how often the queue is looked at. `queue worker` is the
same thing as a process: it publishes what is due, waits `--tick` and looks
again, until it is interrupted.

The schedule of a target is its `--interval`. A target with an interval is
published again when the interval has passed since the last run, and a target
without one (`--interval 0 --manual`) is only published when a person names
it with `--target NAME`. A failed run is not a lost run: the target keeps its
schedule, the failure is the task of the history, and the next run tries
again.

The safety of the push is the safety of the queue. A target that publishes to
a remote branch with unrelated history keeps that branch, the task of the run
is `diverged`, and the run exits 1. Only a target that was given `--force`
overwrites it, and that is a decision of a person, made once, on the target
and not on the run.

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
