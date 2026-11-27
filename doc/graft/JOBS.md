# The jobs of a deployment

Every periodic job of a deployment is a workflow of a Temporal server: the
sweep of the mirror queue, the tiering of its fallbacks, the import of a
remote into the storage core, the backup gate of the storage core, the dumps of
the databases and the gate and the backups of the checkouts of a workstation.

The schedule lives on the server, the work lives on a worker, and the history
of a run is something a person can read: what ran, when, how long it took and
what it did. A job that fails keeps its failure in the history instead of in a
log file that nobody reads, and a job that is retried is retried by the server
rather than by a line of cron.

## The two halves

| Half | What it is | Where it runs |
| --- | --- | --- |
| The schedule | The calendar of the jobs: the cron expression, the workflow to start, the arguments | The Temporal server, in the namespace `graft` |
| The worker | The process that runs the workflows and their activities | The node that holds the files the job works on |

A worker polls one task queue. The worker of the storage core polls
`graft-jobs`; the worker of the workstation polls `graft-jobs-workstation`,
because the files of the checkouts are there and a job that works on a file
must run where the file is.

## The jobs

| Job | Schedule | What it does |
| --- | --- | --- |
| `graft-mirror-sweep` | every minute | publishes the mirror targets that are due, one child workflow per target |
| `graft-mirror-tier` | 04:23 daily | sets the fallback interval of every target by the age of the last change of its repository |
| `graft-remote-import` | every 30 minutes | brings the commits of a remote that moved ahead into the storage core |
| `graft-backup-gate` | 17 minutes past every hour | runs the backup gate of the storage core and writes its report |
| `graft-database-dump` | 03:40 daily | dumps the database of the storage core and proves that the dump can be restored |
| `graft-database-dump-ci` | 03:35 daily | dumps the database of the CI server and proves that the dump can be restored |
| `graft-local-backup-gate` | 23 minutes past every hour | runs the gate of the local repositories of the workstation |
| `graft-local-backup-full` | 02:00 on Sunday | backs the checkouts of the workstation up completely |
| `graft-local-backup-incremental` | 02:00, Monday to Saturday | backs the checkouts up since the last backup |

The minutes are the minutes the cron of the node used. The schedules name the
time zone of the node (`Asia/Shanghai`), because the cron of Temporal is UTC
and a schedule that runs at 17 minutes past the hour in the local clock is a
schedule the operator can recognise.

## The worker

`cmd/graft-worker` is the worker. It has four subcommands:

```shell
graft-worker worker          # run the jobs of the node until it is stopped
graft-worker register        # put the schedules of the jobs on the server
graft-worker unregister      # take them off the server
graft-worker list            # print the schedules and when they run next
graft-worker run JOB         # start one job once, without waiting for its schedule
```

The configuration is read from the command line and from the environment; the
environment wins over the defaults and the command line wins over the
environment. The file of the node that holds it is `~/.config/graft-worker/worker.env`
and it must stay at 0600.

| Variable | What it is |
| --- | --- |
| `GRAFT_WORKER_ADDRESS` | The Temporal server, e.g. `10.126.126.6:7233` |
| `GRAFT_WORKER_NAMESPACE` | The namespace the jobs live in; `graft` by default |
| `GRAFT_WORKER_TASK_QUEUE` | The queue the worker polls |
| `GRAFT_WORKER_QUEUE_FILE` | The file the mirror queue is kept in |
| `GRAFT_WORKER_GRAFT_ADDRESS` | The storage core the mirror queue publishes from |
| `GRAFT_WORKER_GRAFT_TOKEN_FILE` | The file that holds the shared token of the operator |
| `GRAFT_WORKER_GATE_SCRIPT` | The backup gate of the storage core |
| `GRAFT_WORKER_GATE_REGISTRY` | The file that names the origins of the repositories |
| `GRAFT_WORKER_GATE_REPORT_DIR` | The directory the reports of the gate are written to |
| `GRAFT_WORKER_DUMP_SCRIPT` | The script that dumps the database of the storage core |
| `GRAFT_WORKER_DUMP_CI_SCRIPT` | The script that dumps the database of the CI server |
| `GRAFT_WORKER_LOCAL_SCRIPT` | The gate of the local repositories of the workstation |
| `GRAFT_WORKER_LOCAL_ROOTS` | The directories the local gate walks, comma separated |
| `GRAFT_WORKER_LOCAL_FULL_SCRIPT` | The script that backs the checkouts up completely |
| `GRAFT_WORKER_LOCAL_INCREMENTAL_SCRIPT` | The script that backs the checkouts up since the last one |
| `GRAFT_WORKER_IMPORT_SCRIPT` | The script that brings the state of a remote into the storage core |
| `GRAFT_WORKER_IMPORT_REPO` | The repository that is imported, e.g. `quant_mt5.git` |
| `GRAFT_WORKER_IMPORT_REMOTE` | The Git URL the state of the repository is imported from |
| `GRAFT_WORKER_IMPORT_EVERY` | How often the import runs; 30 minutes |

No secret is in that file. The credential of the operator is read from the
file that holds it, and the credentials of the mirrors are read from their own
files when a task runs, so a secret never sits in an environment file, in the
queue or on a command line.

## What a job runs

The mirror jobs are native Go: the worker of the queue is the same code the
`graft mirror queue run` command runs, so a run under Temporal and a run from
a shell do the same thing.

The gate and the dump jobs run the scripts of the node. The script is the
thing that knows the node, and the workflow only owns the calendar, the retry,
the timeout and the history. A script that exits non-zero is a failed run: the
report of the gate holds the detail, and the operator reads the last run of
the schedule.

## The import of a remote

The mirror has two directions. The queue publishes the storage core to a
remote, and this job brings the commits of a remote that moved ahead into the
storage core. Without it the queue reports the target as `diverged`: it does
not push commits it does not have, and the commits of the remote stay in the
remote.

The order of the two steps is the whole point, and both of them are in the
script (`deploy/host-scripts/import-remote.sh`):

1. The objects of the remote are fetched into the repository of the server
   **without moving a reference**: the fetch writes `FETCH_HEAD`, and
   `refs/heads/*` is left alone.
2. The reference moves through a **land**, which is the only way the storage
   core moves one: the server writes the reference and commits the changeset
   to the database and to the object store.

Two ways of doing this are wrong, and both were tried:

- A fetch that moves the reference (`+refs/heads/*:refs/heads/*`) writes
  behind the server's back. The database keeps the old commit, the repository
  of the server has the new one, and the server then believes the branch is
  already where the remote put it: the land answers "already landed" and the
  database is never updated. The reference of the storage core and the
  repository of the server disagree, and `graft storage verify` reports the
  divergence for ever.
- `graft storage import` does not move the references of a repository that is
  imported already. It brings in the changelog and the reviews, and reports
  zero references. It is the way to register a repository, not the way to
  bring a remote in.

## What is not a job of the deployment

The job layer holds the jobs of a **graft deployment**. The timers and the
cron lines of a node that belong to another project are left alone, and the
list is written down so that the scope is a decision and not an accident:

| Job | Where | Why it is not a job of the deployment |
| --- | --- | --- |
| `pg_backup.sh` of the research database | NAS, 03:20 daily | it backs the `research` database of another project, out of the `gitai` database |
| `maxllm-*.timer` | NAS, every few minutes | the health and the sanity of the model gateway of the operator |
| `agents-sync.timer` | NAS and workstation, every 30 minutes | it pulls the shared `~/.agents` repository of the operator, which is not a repository of the deployment |
| `launchpadlib-cache-clean.timer` | workstation | the cache of a python package |
| `cold-backup`, `cold-bulk`, `cold-verify` | NAS, `/etc/cron.d` | the cold backup of the zonefs, owned by root |
| `sysstat`, `e2scrub_all`, `zfs-auto-snapshot` | both nodes | the operating system |

The scripts that a release runs on demand — `check-lane.sh`,
`healthcheck.sh`, `gate-check.sh`, `backup-db.sh` — are not jobs either: they
run when a release runs, and a release is not a calendar.

## Installing it

`deploy/host-scripts/install-worker.sh` puts the worker on a node: it installs
the binary, writes the systemd user unit, starts the service and puts the
schedules of the jobs of that node on the Temporal server. A worker without a
calendar runs nothing, so the install registers the schedules and prints the
list of them as the proof.

```shell
install-worker.sh --from ~/opt/git-ai/bin/graft-worker
```

The register is a create-or-update, so the install may run again after a change of
the configuration: the runs a schedule had keep their history. `--no-register`
leaves the calendar of the server alone, for the node whose jobs another node
already registered.

The script never writes the environment file: it holds the configuration and
the paths of the credentials, and it must stay at 0600.

## The cut over

The cron of a node is the old way to run a job. A node runs both for a while:
the cron is commented out, the schedules are registered, and the first runs
of the schedules are watched. The comment in the crontab keeps the line, so
the old way can be put back:

```cron
# temporal: 17 * * * * ~/opt/git-ai/deploy/host-scripts/backup-origin-gate.sh --sync ...
```

The queue of the mirrors is the one thing that both the worker and the old
cron write, so the cut over is done with the cron line of the sweep commented
out first. The queue file itself is safe: the store takes a lock of its own
around every read and write of the file, so two writers take turns and
neither loses a change.
