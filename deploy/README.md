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
ubuntu 24.04): the bundled Git is dynamically linked, and the server runs in an
`ubuntu:24.04` container on the NAS. A build from an older distribution will
not start there — that is why the NAS runs the server in a container at all.

The bundled Git comes from `https://github.com/git/git.git` (GitLab is
unreachable from this network). Its MVCC build exists only on GitLab and is
replaced by the default build: these lanes do not use Git's MVCC reference
backend. `build-release.sh` reads the default revision from the Makefile and
passes it as the MVCC revision, so the two stay in step.

### The gate

`deploy/integration-test.sh staging <version>` runs
`host-scripts/integration-suite.sh` on the NAS: fourteen cases over the lane's
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
ssh oliveagle@oleNas01 'bash -s' <<'SH'
LANE_DIR=/home/oliveagle/opt/git-ai
LANE_CONTAINER=git-ai-gitaly
LANE_PROGRAMS="git-ai-cache git-ai-cache-tcp"
VERSION=legacy-<date>        # or any directory under releases/
# run host-scripts/switch-current.sh with these in the environment
SH
```

In practice, run `deploy/promote.sh <rather-older-version>` after reinstalling
it in staging, or call `switch-current.sh` directly — it is idempotent.

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
`reports/promote-<version>-<ts>.json`. That is a per-promotion safety net, not
a backup schedule: nothing dumps `gitai` between promotions.

## Known gaps

- **Nothing backs up `gitai` on a schedule.** The NAS's daily `pg_backup.sh`
  dumps the `research` database only; the storage core's changesets, refs and
  changelog live in `gitai` (and the packs in RustFS), so only a promotion
  leaves a dump behind.
- **The object store account is shared** by both lanes (see above).
- **`graft-cache` logs are not rotated**; they are appended under the lane's
  `logs/`.
- The staging lane keeps its test repositories. They are named `it-<run>-*`,
  and `graft storage gc --removed` collects removed ones.
