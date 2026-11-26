# The observer: the read model of the storage core

`graft-observer` is a service of this repository. It reads the storage core
and serves the picture of the fleet to clients that show it — today the
git-ai-view frontend, tomorrow anything else that wants the same picture.

It exists because a client that shows the fleet must not talk to the
databases, to the worker hosts or to a model. The observer owns those
dependencies: a client speaks HTTP and JSON to it and never reaches a
database, a worker host or a model endpoint.

## What it reads

- the storage core: repositories, refs, changesets, the changelog, reviews,
  checkpoints, the removed repositories, the nodes and the mirror targets;
- the operational state of the mirrors and the backups, which the worker host
  produces as files and reports;
- the CI database, which Woodpecker owns;
- the model that names the galaxies, when one is configured.

## What it never does

- it opens the storage core read-only: it never writes a row and never
  applies a migration;
- it never holds a credential in an answer. The snapshot names the database by
  host, port and name, never by the DSN it was given;
- it never lets a slow or unreachable dependency take the API down: the last
  good picture is served with a warning.

## The endpoints

| Endpoint | What it answers |
| --- | --- |
| `GET /api/snapshot` | The whole fleet: source, repositories, nodes, mirrors, events, counters |
| `GET /api/events` | The activity as a stream (SSE); a client that reconnects with a cursor gets what it missed |
| `GET /api/health` | The state of the service, of the storage core, of the mirrors, of the backups and of the CI |
| `POST /api/refresh` | Reads what changed right now |
| `GET /api/galaxy-names` | The galaxy labels the namer holds, with the meter of the budget |
| `POST /api/galaxy-names` | Asks the namer to name the families that changed |
| `GET /api/mirrors` | The mirror fabric: the plan of the queue, the measured state of the server, the runs |
| `GET /api/backups` | The verdict of the backup gate, the lag of every repository and the disks |
| `GET /api/ci` | The telemetry of the CI server: the pipelines, the repositories and the agents |
| `POST /api/actions/repo-create` | Registers a repository, with the credentials of the server |
| `POST /api/actions/land` | Commits a workspace and lands it on a branch |
| `POST /api/actions/mirror-push` | Publishes a repository to a mirror |

The three read endpoints of the pages hold their own cache: the CI telemetry
for eight seconds, the mirror telemetry for ten and the backup report for a
minute. A page that is read often costs one collection, not one collection
per request.

## The mirrors, the backups and the CI

The mirror telemetry joins three authorities:

- the queue of the operator (`mirror-queue.json`), which holds the plan: the
  interval of every target, the next run and the last tasks;
- the graft server, which holds the measured state: how far a target is
  behind, when it published last. The observer asks the server with the SDK
  and falls back to the tables of the storage core when the server cannot be
  reached;
- the backup gate, which reads the origins back and says whether a backup
  counts. Its report is a file of the node.

The observer reads the queue and the report as files of the node it runs on.
No client ever asks a worker host anything over ssh.

## The writes

The client asks; the server does. The browser holds a token of the observer
and nothing else. The observer holds the token of the graft server, runs the
`graft` client with it and redacts the token from every line it shows.

## The events

Every change of the storage core becomes an event. The events are kept in a
ring of 1600 entries in memory and in a journal (`activity.jsonl`, rotated at
24 MB) in the state directory, so a service that restarts keeps its history
and a client that was away can catch up.

## The configuration

| Variable | What it is |
| --- | --- |
| `GRAFT_OBSERVER_DATABASE` | The storage core: `postgres://…` or `sqlite://…` |
| `GRAFT_OBSERVER_LISTEN` | The address the HTTP server listens on |
| `GRAFT_OBSERVER_POLL_INTERVAL` | How often the storage core is read; 900 ms by default |
| `GRAFT_OBSERVER_STATE_DIR` | Where the journal of the events is kept |
| `GRAFT_OBSERVER_ADDRESS` | The address of the graft server, shown as the origin of the writes |
| `GRAFT_OBSERVER_STATIC` | The directory of the built frontend, when the service hosts it |
| `GRAFT_OBSERVER_VERSION` | The version the health endpoint reports |
| `GRAFT_OBSERVER_TOKEN` / `GRAFT_OBSERVER_TOKEN_FILE` | The token a client must carry to read the API |
| `GRAFT_OBSERVER_ACTION_TOKEN` | The token a client must carry to ask for a write |
| `GRAFT_OBSERVER_GRAFT_BIN` | The `graft` client the observer uses to write |
| `GRAFT_OBSERVER_GRAFT_USER` | The identity the writes are made with |
| `GRAFT_OBSERVER_STORAGE` | The storage the writes address |
| `GRAFT_OBSERVER_QUEUE_PATH` | The mirror queue of the node |
| `GRAFT_OBSERVER_REPORTS_DIR` | The directory of the reports of the backup gate |
| `GRAFT_OBSERVER_DUMPS_DIR` | The directory of the dumps of the storage core |
| `GRAFT_OBSERVER_CI_DATABASE` | The database of the CI server, read only |
| `GRAFT_OBSERVER_CI_URL` | The web base of the CI server, so a pipeline row can link to its own page |
| `GRAFT_OBSERVER_MIRROR_TTL` | How long a reading of the mirror telemetry is served |
| `GRAFT_OBSERVER_CI_TTL` | How long a reading of the CI telemetry is served |
| `GRAFT_OBSERVER_LABEL_URL` | The endpoints of the naming model, e.g. `http://host:3795/v1/responses\|model`, comma separated |
| `GRAFT_OBSERVER_LABEL_MODEL` | The model the endpoints expect, when the URL does not name one |
| `GRAFT_OBSERVER_LABEL_KEY` / `GRAFT_OBSERVER_LABEL_KEY_FILE` | The credential of the naming model |
| `GRAFT_OBSERVER_LABEL_DAILY_CALLS` | How many calls the naming model may make a day; 32 by default |
| `GRAFT_OBSERVER_LABEL_MIN_INTERVAL_MS` | The smallest gap between two calls; five seconds by default |
| `GRAFT_OBSERVER_LABEL_TIMEOUT_MS` | How long one call may take; 45 seconds by default |
| `GRAFT_OBSERVER_LABEL_RETRY_AFTER_MS` | How long a family that failed is left alone; six hours by default |

## Where it runs

Next to the server, on the node host. The mirror queue, the reports of the
backup gate and the dumps of the storage core are files of that host, and the
CI database is on the same machine: reading them there costs no ssh and no
remote process. The one thing that stays remote is the gate of the local
backups of the workstation, and it is pulled in on a slow schedule.

## The cost of the model

Naming the galaxies costs a call to a model, so it is bounded: a hard ceiling
of calls per day, a minimum interval between two calls, and a cache keyed by
the membership of the galaxy. A fleet that does not change costs nothing.

The client asks for a family to be named and does not wait for it: it posts the
family with its members, its stacks, its agents and its mirrors, and the
namer answers when the budget allows. A family that keeps its members is
never asked about again — the name is cached by the membership, on disk, next
to the journal of the events — and a family that failed is left alone for
hours instead of being retried. The names are handed to the client by
`GET /api/galaxy-names`, which is also the meter: the calls of the day, the
ceiling, the last call and the last error.
