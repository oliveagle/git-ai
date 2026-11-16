# The backup of a repository

Every repository in the storage core must have a backup origin, and a backup
counts only when the origin has been read back and carries the same commits.

The rule has no exception. A repository without an origin is a failure of the
gate, not a matter of taste: the storage core is the only copy of the history,
the reviews and the verification state of a repository, and the nodes that
serve it share one database and one object store. A node that dies loses
nothing, because the storage core has it. A storage core that dies loses
everything, and only an origin outside it survives.

## What an origin is

An origin is any Git host that holds the history of the repository:

- a repository on GitHub, GitLab, Gitee or a self-hosted server, or
- a bare repository on another machine of the operator's own.

An origin is named in one file, `backup-origins.json`, which holds no secret:
an origin names the environment variable that carries its credential, the way
the mirror queue names it. The file lives beside the mirror scripts of the
node and not in the repository, because it names the hosts of the operator.

```json
{
  "version": 1,
  "repositories": {
    "shop.git": {
      "origins": [
        {
          "name": "github",
          "url": "git@github.com:group/shop.git",
          "branches": ["*"]
        }
      ]
    }
  }
}
```

`branches` is the allowlist of the branch patterns that may reach the origin,
in the same shape as `graft mirror push --branch`: no `refs/heads/` prefix,
and `*` is allowed. The tags always travel with the branches, because a tag
carries the commits that a branch may name.

## What counts as a backup

A push is not a backup. A push is an attempt, and it can fail halfway, be
throttled by the host, or be cut off by the network. A backup counts when the
origin has been read back and holds the commits of the authoritative refs:

- every branch the origin may receive is on the origin,
- with the same commit id as the storage core holds for it,
- and the tags are on the origin too,
- and the origin holds nothing else, because a ref that the storage core does
  not know is a divergence that a person has to look at.

Anything else is a failure, and the failure is what the operator reads.

## The gate

`deploy/host-scripts/backup-origin-gate.sh` is the gate. It runs on the node
host, reads the authoritative refs from the storage core, reads the registry
of the origins, and then asks every origin what it holds.

```sh
backup-origin-gate.sh                 # verify only
backup-origin-gate.sh --sync          # publish what is missing, then verify
backup-origin-gate.sh --repo shop.git # one repository
```

The gate fails, and exits non-zero, when

- a repository of the storage core has no origin in the registry,
- an origin cannot be reached,
- an origin does not carry a branch that it may receive, or carries it with
  another commit id,
- an origin carries a ref that the storage core does not know.

A gate that nobody runs is a wish. The gate is run from a schedule, and the
schedule is what turns a backup into a fallback: a run that failed is not a
lost run, the next run tries again, and the history of the runs says what
happened.

## The schedule

The gate belongs to a cron job, next to the mirror jobs it verifies:

```cron
17 * * * * ~/opt/git-ai/bin/backup-origin-gate.sh --sync >> ~/opt/git-ai/logs/backup-origin-gate.log 2>&1
```

The `--sync` run publishes first and verifies second, so a repository that
drifted is repaired by the run and the gate still fails when the repair did
not work. The exit status is the report: zero when every repository has an
origin and every origin is in sync, non-zero otherwise.

The publish uses `deploy/host-scripts/mirror-push.sh`, which reads the
authoritative refs from the storage core, copies the objects out of the node
copy, pushes the heads and the tags, and then reads the remote back and
compares it with what it wanted. A push that cannot be proven is a failed
push.

## The recovery

An origin is history, not the review state: `refs/graft/` is never published,
because a mirror carries the commits, not the verification of the commits.
The recovery of a lost storage core is therefore:

1. clone the origin into a plain Git repository,
2. import it into a storage core with `graft storage import`,
3. the reviews and the changelog of the lost core are gone, and the history
   is back.

That asymmetry is the reason the database of the storage core is dumped on
its own schedule: the mirror keeps the commits, the dump keeps the state.

## The mirror queue

The mirror queue of the client is the same idea with a history: it holds the
targets, publishes the ones that are due, and leaves a task behind for every
run. The registry of this document is the shape of one target, so the two fit
together: the registry is the place where the operator declares the origins,
and the queue is the place where the publishing happens.
