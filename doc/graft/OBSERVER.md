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
| `GET /api/health` | The state of the service and of the storage core |
| `POST /api/refresh` | Reads what changed right now |
| `GET /api/galaxy-names` | The galaxy labels the namer holds |

## The events

Every change of the storage core becomes an event. The events are kept in a
ring of 1600 entries in memory and in a journal (`activity.jsonl`, rotated at
24 MB) in the state directory, so a service that restarts keeps its history
and a client that was away can catch up.

## The configuration

| Variable | What it is |
| --- | --- |
| `GRAFT_OBSERVER_DATABASE` | The storage core: `postgres://…` or `sqlite://…` |
| `GRAFT_OBSERVER_LISTEN` | The address the service listens on |
| `GRAFT_OBSERVER_POLL_INTERVAL` | How often the storage core is read; 900 ms by default |
| `GRAFT_OBSERVER_STATE_DIR` | Where the journal of the events is kept |
| `GRAFT_OBSERVER_ADDRESS` | The address of the graft server, shown as the origin of the writes |
| `GRAFT_OBSERVER_STATIC` | The directory of the built frontend, when the service hosts it |
| `GRAFT_OBSERVER_VERSION` | The version the health endpoint reports |

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
