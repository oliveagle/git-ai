# Releasing git-ai: the staging lane and the gate

This directory turns "build it, try it, then put it on the NAS" into a
procedure with an artifact, a gate and a rollback. It is the runbook: read it
before the first release you cut, and follow it every time.

The production service is the one that was deployed by hand on `oleNas01`:
clients talk to `tcp://192.168.2.190:8076`, its data lives in the PostgreSQL
database `gitai` and the RustFS bucket `git-ai-objects`. It is never the place
a release is tried out.

## The two lanes

A lane is a whole service: a directory, a release it runs, a token, a
database role and database, a bucket, two ports, a container and two cache
tiers. The two lanes share the machine and the storage *server*, nothing else.

| | production | staging |
|---|---|---|
| directory | `~/opt/git-ai` | `~/opt/git-ai-staging` |
| server port | 8075 | 8175 |
| cache port (clients) | 8076 | 8176 |
| cache metrics | 9237 (unix 9236) | 9337 (unix 9336) |
| token | `tokens/shared` | `tokens/shared` — a different one |
| PostgreSQL | role `temporal`, database `gitai` | role `temporal_staging`, database `gitai_staging` |
| bucket | `git-ai-objects` | `git-ai-objects-staging` |
| node id | `oleNas01` | `oleNas01-staging` |
| container | `git-ai-gitaly` | `git-ai-gitaly-staging` |
| supervisor programs | `git-ai-cache`, `git-ai-cache-tcp` | `git-ai-cache-staging`, `git-ai-cache-staging-tcp` |
| supervisor ini | `git-ai.ini` | `git-ai-staging.ini` |

The names live in `lanes.env`; the scripts read them from there and nowhere
else.

Isolation is not a promise, it is checked. `host-scripts/check-lane.sh` refuses
to start a lane whose `config.toml` names the other lane's database, bucket,
port or role, and whose token equals the other lane's. `integration-suite.sh`
adds the proof from the outside: the test repositories must appear in this
lane's database and in this lane's bucket, and in neither of the other lane's.

The object store account is still shared by both lanes (the store has no
per-lane account yet). The buckets are separate, and the suite checks that the
writes landed in the right one. Giving staging its own account is the next
hardening step.

## The road

```sh
deploy/pipeline.sh                 # build, deploy to staging, run the gate
deploy/pipeline.sh --promote       # ... and replace production when it passes
```

or, step by step, when a release needs a second look:

```sh
deploy/build-release.sh            # dist/git-ai-release-<version>.tar.gz
deploy/deploy-lane.sh staging dist/git-ai-release-<version>.tar.gz
deploy/integration-test.sh staging <version>    # the gate
deploy/promote.sh <version>        # re-checks the gate, installs, health-checks
deploy/rollback.sh prod [version]  # go back to a release that is already installed
```

`promote.sh` takes no bundle from this machine: it installs the tarball the
staging lane ran, so the bytes that passed the gate are the bytes production
gets.

### What a release is

`dist/git-ai-release-<version>/` holds `bin/gitaly` (with its three bundled Git
builds), `bin/graft`, `bin/graft-cache`, `bin/mkbucket`, a `VERSION`, a
`RELEASE` file naming the commit and the Git revisions, and `SHA256SUMS`. The
tarball of that directory is what a lane installs; `install-release.sh` refuses
one whose checksums do not match.

The server is built on a machine with glibc 2.39 (`oleworkstation01`,
ubuntu 24.04) and runs in an `ubuntu:24.04` container on the NAS: the bundled
Git is dynamically linked, and a build from an older distribution will not
start there. That is the one coordinate that has to match — what the Git
reaches for at runtime is the image's job, so the bundle does not have to be
cut in a container of the target distribution.

### The lane image

Both lanes start their container from `LANE_IMAGE` (`lanes.env`), which
`switch-current.sh` builds from `ubuntu:24.04` the first time it is missing:

```
FROM ubuntu:24.04
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates libcurl3t64-gnutls libcurl4t64 openssh-client
# the container runs as the deploy user, and ssh refuses a uid it cannot
# name ("No user exists for uid 1002")
RUN groupadd -g 1003 lane \
 && useradd -u 1002 -g 1003 -M -d /home/oliveagle -s /bin/sh lane
```

The release bundles its own Git, but that Git is not static, and a bare
`ubuntu:24.04` carries none of what it reaches for at runtime:

| package | what breaks without it |
|---|---|
| `libcurl3t64-gnutls` | `git-remote-https` dies with "cannot open shared object file" — every HTTPS remote, so every mirror push |
| `libcurl4t64` | the same helper, built on a machine whose libcurl is the OpenSSL flavour, cannot load |
| `ca-certificates` | an HTTPS push fails to verify the mirror's certificate (`CAfile: none`) |
| `openssh-client` | a mirror reached with `--ssh-key`, and `GIT_SSH_COMMAND=ssh`, find no `ssh` |

A missing library is invisible until something reaches for the network: the
server starts, serves, clones and lands without ever loading `git-remote-http`.
That is why `check-lane.sh` asserts the image carries these four, and why
`container_runtime` runs the bundled helper inside the container. Rebuild the
image with `LANE_IMAGE_REBUILD=1` after changing the package list.

The bundled Git comes from `https://github.com/git/git.git` (GitLab is
unreachable from this network). Its MVCC build exists only on GitLab and is
replaced by the default build: these lanes do not use Git's MVCC reference
backend. `build-release.sh` reads the default revision from the Makefile and
passes it as the MVCC revision, so the two stay in step.

### The gate

`deploy/integration-test.sh staging <version>` runs
`host-scripts/integration-suite.sh` on the NAS: fifteen cases over the lane's
cache port, plus the checks only the host can make. It fails unless all of them
pass:

| case | what it proves |
|---|---|
| `repo_create_clone` | the operator token creates a repository and a client clones it |
| `status_clean` | a fresh workspace reports no change |
| `commit_land_read` | a commit lands and `show`/`cat` read it back; the changelog rows exist in this lane's database |
| `history_blame_search` | history, blame and content search agree on a file |
| `review_flow` | a review opens on a pending commit and is approved |
| `update_revert` | the workspace follows the branch and discards a local change |
| `second_writer` | a second writer lands beside the first |
| `stale_writer_refused` | a writer whose base is behind cannot lose the other's history |
| `storage_verify` | storage state, connectivity verification and a dry-run GC are clean |
| `token_isolation` | the other lane's token is refused |
| `data_isolation` | the test repositories reached this lane's database and bucket, not the other's |
| `cache_serves` | repeated reads are served by the cache |
| `restart_survives` | a restart keeps every changelog row and the state still answers |
| `cache_rebuild` | a deleted local cache is rebuilt from the storage core (staging only) |
| `container_runtime` | the container can load the bundled `git-remote-https`, and carries the CA bundle and `ssh` |

The report lands in `<lane>/reports/integration-<version>-<run>.json` and names
the release, the lane, each case and the totals. `host-scripts/gate-check.sh`
reads it back and refuses a promotion unless:

1. a report exists for exactly this version and says `PASS`,
2. it counts zero failures,
3. it is younger than `MAX_AGE_HOURS` (24 by default),
4. the staging lane still runs that version,
5. the tested bundle is still on the host.

A stale pass, a pass for another version or a pass from a lane that has since
been redeployed is not a pass.

### What a deploy does to a lane

1. the bundle is staged and its tools extracted to `<lane>/.tools`;
2. `prepare-lane.sh` creates what is missing — directory, token, PostgreSQL
   role and database, bucket, `config.toml`, `run-gitaly.sh`, the two supervisor
   programs in the lane's own ini — and never rotates a secret or rewrites a
   `config.toml` that already exists. `lanes.env` names the ini; production
   keeps the `git-ai.ini` it has had since before the lanes existed;
3. `check-lane.sh` refuses a lane whose identity or isolation does not hold;
4. `install-release.sh` verifies the checksums and unpacks into
   `releases/<version>/`;
5. `switch-current.sh` moves the `current` symlink, recreates the container and
   restarts the two cache tiers — the switch itself is one `mv` of a symlink;
6. `healthcheck.sh` probes container, socket, both ports, cache stats, the
   version the process reports against the release on disk, the storage core,
   the lane's database and the bucket. Failure rolls the lane back to the
   release that ran before.

`bin` is a symlink to `current/bin`, so a promotion is one symlink swap and a
restart, and the previous release stays installed. The first deploy on the
production lane migrates its unversioned `bin/` into `releases/legacy-<date>/`
and keeps it for a rollback.

### Rolling back

```sh
deploy/rollback.sh prod                            # the newest installed release before the current one
deploy/rollback.sh prod legacy-20261114-011107     # or one by name
```

It switches `current` to a release that is already installed, restarts the
container and the cache tiers, and runs the same health check a deploy runs —
nothing is built or downloaded, so a rollback cannot fail for want of a bundle.
`deploy-lane.sh` performs the same switch on its own when a release fails its
health check, and `promote.sh` refuses a release the gate has not passed, so a
rollback is the only way a lane goes backwards.

`releases/legacy-<date>` is the release a lane ran before it adopted this
layout. It is a rollback target like any other, and rolling back to it was
verified to read data written by the newer release.

## Prerequisites

On the machine that builds:

- Go 1.26 (the toolchain directive fetches it), `make`;
- `meson >= 0.61` and `ninja` for the bundled Git (`sudo apt install meson ninja-build`);
- `libcurl4-openssl-dev libexpat1-dev gettext libssl-dev zlib1g-dev libpcre2-dev`;
- glibc 2.39 (ubuntu 24.04) — see above;
- push access to the fork, and ssh access to `oleNas01` as `oliveagle`.

On `oleNas01`: docker (the user is in the `docker` group, there is no sudo),
`~/.supervisord` running with the `configs.d` include, Pigsty's
`pigsty-postgres` container and the `rustfs` container.

## The database

A promotion dumps the production database before it installs anything
(`host-scripts/backup-db.sh`, through the PostgreSQL container), keeps the
newest ten dumps per release in `~/backups/git-ai/`, and names the dump in
`reports/promote-<version>-<ts>.json`. That is a per-promotion safety net.

The schedule is `host-scripts/pg-dump.sh`: it reads the database the lane's
`config.toml` names and runs the host's `~/bin/pg_backup.sh` against it — the
dump, a check that the archive is a readable `pg_dump` TOC, a restore drill
that compares the row count of every user table on both sides, and a rotation
of the newest dumps — into `~/backups/pg-gitai/`. It is a read of the lane, so
it needs no window. The crontab of the host runs it once a day:

```sh
40 3 * * * /home/oliveagle/opt/git-ai/deploy/host-scripts/pg-dump.sh --lane prod \
  >> /home/oliveagle/backups/pg-gitai/backup.log 2>&1
```

What it covers is what the storage core keeps in PostgreSQL: the changeset log,
the refs, the mirror targets and their publications, and the policies. The
packs are in the object store and the repositories on disk; those are what
`backup-origin-gate.sh` reads back.

## The backup of a repository

Every repository in the storage core must have a backup origin, and a backup
counts only when the origin has been read back and carries the same commits.
The rule has no exception; `doc/graft/BACKUP.md` says what an origin is and
what makes a backup count.

The origins are named in `~/opt/git-ai/mirror/backup-origins.json` (the
template is `backup-origins.example.json`), and two host scripts enforce the
rule:

| Script | Runs on | Does |
|---|---|---|
| `host-scripts/backup-origin-gate.sh` | the node host | reads the authoritative refs from the storage core and asks every origin what it holds; exits non-zero when a repository has no origin, an origin cannot be reached, or an origin is out of sync |
| `host-scripts/mirror-push.sh` | the node host | publishes one repository to one remote from the node host, then reads the remote back |
| `host-scripts/local-backup-gate.sh` | the workstation | walks the checkouts, and exits non-zero when a repository has no origin or the origin does not hold its commits |

The checkouts on the workstation obey the same rule: the unit of that check is
the commit, not the name of a branch, so a renamed or deleted branch does not
lose its backup and a branch that is only behind the origin is not a failure.
A checkout whose origin is a one-way mirror of the storage core is named in
`~/etc/backup-origins.local.json` (`backup-origins.local.example.json`), and
the gate only verifies it: a commit the mirror does not hold has to land in the
storage core first, because the mirror prunes what the storage core does not
know.

```sh
~/opt/git-ai/deploy/host-scripts/backup-origin-gate.sh                  # verify only
~/opt/git-ai/deploy/host-scripts/backup-origin-gate.sh --sync            # publish, then verify
~/opt/git-ai/deploy/host-scripts/backup-origin-gate.sh --repo shop.git   # one repository
```

The gate is run from cron, next to the mirror jobs it verifies; `--sync`
repairs what drifted and the run still fails when the repair did not work. A
gate that nobody runs is a wish, and a backup that was pushed once by hand is
not a fallback.

## The mirror queue

A mirror is declared, not remembered: `host-scripts/mirror-queue-seed.sh` reads
the table at its top — one line per repository and destination — and puts every
line into the mirror queue as a target. Adding a target is also what registers
it with the server, and the server decides the next push of a repository on
what its targets last published (`doc/graft/MIRROR.md`). A repository whose
mirror was never declared therefore counts as having none, which the policy of
the production lane refuses:

The table is the declaration, and re-running the script applies it: a target the
queue already holds is restated — URL, branches, interval — while the outcome of
its last publication stays, because that outcome is what the policy reads.
`graft mirror queue update NAME` is the same restatement by hand, with the flags
of `add`.

```toml
[graft.mirror]
required = true
warn_lag = 10
```

The worker is one cron line, every five minutes: `host-scripts/
mirror-queue-run.sh` publishes the targets whose interval has passed, and the
queue decides what is due.

```sh
~/opt/git-ai/deploy/host-scripts/mirror-queue-seed.sh            # declare, idempotent
~/opt/git-ai/deploy/host-scripts/mirror-queue-run.sh             # publish what is due
~/opt/git-ai/deploy/host-scripts/mirror-queue-run.sh --all       # publish everything
~/opt/git-ai/deploy/host-scripts/mirror-queue-run.sh --target github-laya-workflow
graft mirror status --repo laya-workflow.git --branch main       # what a push will get
```

The queue file is `~/opt/git-ai/mirror/mirror-queue.json`. It holds no secret:
a target names the variable that carries its token or the path of its key, and
the value is read when the task runs.

The file is named by `--queue-file`, then by `GRAFT_MIRROR_QUEUE`, then by the
default of the user (`~/.config/graft/mirror-queue.json`) — and the worker sets
`GRAFT_MIRROR_QUEUE` to the path above, so a shell that runs `graft mirror queue
run` by hand reads a different, empty queue unless it sets the variable or runs
the wrapper. The run says which file it read, so an empty queue does not read
as one with nothing to do.

The production lane's targets reach GitHub over SSH on port 443
(`ssh://git@ssh.github.com:443/OWNER/REPO.git`), because this network drops the
connection to `github.com:443` often enough that an HTTPS push fails more often
than it succeeds; the workstation's copies are reached over SSH as well. The
staging lane keeps the policy off: the integration suite creates repositories
with no mirror and pushes to them on purpose.

## Known gaps

- **Only GitHub and the workstation are mirrors today.** A Gitee, GitLab or
  Codeup target needs a token for that host and nobody has put one in the
  environment of the worker; `mirror-queue-seed.sh` gains a line per host and a
  token variable, and `graft mirror push` needs no new code for it. Until then
  a repository whose only mirror is GitHub is one host away from having none.
- **The object store account is shared** by both lanes (see above).
- **`graft-cache` logs are not rotated**; they are appended under the lane's
  `logs/`.
- The staging lane keeps its test repositories. They are named `it-<run>-*`,
  and `graft storage gc --removed` collects removed ones.
