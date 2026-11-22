# PLAN13: archive, cleanup observation and the super admin's purge

Status: approved. The removal of a repository is refused in every mode. A
repository is archived first; cleanup closes its reads and starts an
observation period of at least seven days; only a super admin purges it after
that period.

Scope: the storage core (`[graft.storage]`), the gate of the repository
lifecycle and the `StorageCoreService` RPCs that retire a repository. The
archive needs the authoritative storage mode: it is the state of the database.

## 1. The problem

`RepositoryService.RemoveRepository` destroys a repository at once. A wrong
relative path, a script that loops over the wrong names, or a client that
confuses two repositories is not recoverable: the row is gone, the local copy
is gone, and the objects in the bucket go `gc_grace` later. GitLab's own
removal is a UI action that a human confirms; a Graft server is driven by
scripts and agents too, so a single call must not be able to destroy history.

The replacement is a lifecycle with a stop on each step:

1. **archive** — the repository stays in the database and in the bucket. Its
   writes are frozen; its reads stay open. A mistake can be undone.
2. **cleanup** — after `archive_retention`, a super admin closes the reads and
   starts `cleanup_observation` (seven days minimum). This step is
   irreversible.
3. **purge** — only the super admin may delete the row, and only after the
   observation period. The deletion records a tombstone; the garbage collector
   deletes the objects after `object_expiration` (never shorter than
   `gc_grace`).

There is no way back from a purge.

## 2. Invariants

- **ARCHIVE RULE.** A repository is never removed. The `lifecycle.Guard`
  refuses `/gitaly.RepositoryService/RemoveRepository` with
  `FailedPrecondition` in every mode, before authorization and before the
  handler runs. The gate refuses the same call for every caller, agents and
  the operator included. `refuseRemoval` in the gate and the guard are both
  needed: the guard covers the RPC server, the gate covers the storage core
  paths that reach the same decision.
- **FROZEN MEANS FROZEN.** An archived repository accepts no changeset: the
  row is locked in the transaction that checks `archived_at`, so the check and
  the write are one decision (`ErrRepositoryArchived`). Reads keep working
  until cleanup starts.
- **READ-CLOSED MEANS NO READS.** After cleanup starts, the gate refuses every
  repository-scoped accessor and maintenance RPC. `StorageCoreService` passes
  through the gate, so `GetStorageState`, cleanup and purge remain available.
- **ONLY ADMIN CHANGES THE LIFECYCLE TOWARDS DELETION.** `StartRepositoryCleanup`
  and `PurgeRepository` require `identity.SourceAdmin`, which only
  `graft.admin_token` (or `graft.admin_token_file`) produces. The shared token
  and agents are refused with `PermissionDenied`.
- **NO EARLY DELETE.** Cleanup cannot start before `archived_at +
  archive_retention`; purge cannot run before `read_closed_at +
  cleanup_observation`. Both deadlines are checked on the database clock, in
  the transaction that makes the change. An early call writes nothing.
- **CLEANUP IS IRREVERSIBLE.** After the reads are closed,
  `UnarchiveRepository` is refused and `StartRepositoryCleanup` refuses a
  second start.
- **THE TOMBSTONE FEEDS THE GC.** Deleting the row fires
  `graft_repositories_tombstone`, which writes the repository's
  `graft_gc_removed` row in the same transaction. The removed-repository sweep
  waits `object_expiration`, with `gc_grace` as its floor, before it deletes
  the bucket prefix. An OSS lifecycle rule can use the same period to move
  objects to a low-frequency storage class before that final deletion.
- **THE ARCHIVE IS AUTHORITATIVE.** Archive, unarchive, list, cleanup and purge
  need the authoritative mode (`FailedPrecondition` otherwise): the state of
  the repository is the database's, and shadow mode has no authority over it.

## 3. State machine

```
live ──archive──▶ archived+readable ──cleanup──▶ archived+read-closed ──observation──▶ purgeable
  ▲                     │                              │
  └────unarchive────────┘                              x (irreversible)
                                                        └──purge (super admin)──▶ gone
```

| State | Row | Writes | Reads | Lifecycle fields |
|---|---|---|---|---|
| live | present | yes | yes | all lifecycle fields null/false |
| archived | present | **no** (`ErrRepositoryArchived`) | yes | `archived_at`, `archived_by`; `read_closed_*` and `purge_after` null |
| cleanup started | present | **no** | **no** | `read_closed_at`, `read_closed_by`, `purge_after` set |
| purged | **gone** | — (`ErrRepositoryNotFound`) | no | tombstone row in `graft_gc_removed` |

`UnarchiveRepository` from `live` is `ErrRepositoryNotArchived`; `Archive`
from `archived` is `ErrRepositoryArchived`; after cleanup starts both are
refused. `Purge` from a readable archive is `ErrRepositoryNotReadClosed`;
`Purge` from `gone` is `NotFound`.

## 4. Storage

The migration `20261117000000_graft_storage_archive` adds five columns to
`graft_repositories`:

| Column | Meaning |
|---|---|
| `archived_at` | database time of the archive; null on a live repository |
| `archived_by` | identity that archived it (`shared`, `admin`, or an agent name) |
| `read_closed_at` | database time when cleanup closed the reads |
| `read_closed_by` | super-admin identity that started cleanup |
| `purge_after` | end of the cleanup observation; null until cleanup starts |

`PurgeRepository` deletes the row; the trigger `graft_repositories_tombstone`
inserts into `graft_gc_removed` in the same transaction. The garbage
collector's removed-repository sweep reads that table, so the objects are
deleted `object_expiration` after the purge, never before `gc_grace`.

## 5. RPCs and CLI

`StorageCoreService` (proto `storage_core.proto`):

| RPC | Caller | Answer |
|---|---|---|
| `ArchiveRepository` | operator or agent (shared token, agent key) | `archived_at`, `archived_by` |
| `UnarchiveRepository` | operator or agent | empty; refused after cleanup starts |
| `ListArchivedRepositories` | operator or agent | the archived repositories and their cleanup state |
| `StartRepositoryCleanup` | **super admin only** | `read_closed_at`, `read_closed_by`, `purge_after` |
| `PurgeRepository` | **super admin only** | the repository ID; the local copy is moved to the storage's temporary directory |

`GetStorageStateResponse` carries `archived`, `read_closed`, `read_closed_at`
and `purge_after`, so a tool sees the state of one repository without listing
the storage.

```shell
graft storage archive -repo R
graft storage unarchive -repo R
graft storage archived -storage S
graft storage cleanup-start -repo R -admin-token "$GRAFT_ADMIN_TOKEN"
graft storage purge -repo R -admin-token "$GRAFT_ADMIN_TOKEN"
```

The CLI deliberately requires the explicit `-admin-token` / `GRAFT_ADMIN_TOKEN`
for cleanup and purge. It does not send an agent key on those commands.

## 6. What a refusal looks like

```text
$ graft storage purge -repo a.git
storage cleanup and purge need -admin-token or GRAFT_ADMIN_TOKEN

$ graft storage cleanup-start ... -admin-token "$SHARED_TOKEN"
rpc error: code = PermissionDenied desc = starting a cleanup needs the super
admin's token (graft.admin_token)

$ graft storage purge ... -admin-token "$GRAFT_ADMIN_TOKEN"   # too early
rpc error: code = FailedPrecondition desc = the repository is read-closed until
2027-05-16T00:00:00Z: it may be purged then

$ (any client) RepositoryService.RemoveRepository
removing a repository is not supported: archive it instead (graft storage
archive), and let a super admin purge it once the archive retention has passed
```

## 7. Limits

- The times are server settings, not per-repository ones.
- A purged repository's name can be created again; it is a new repository with
  a new ID.
- Graft does not call an OSS lifecycle API. `object_expiration` is the
  application-side final-deletion deadline; configure the bucket rule with the
  same period when objects are first moved to a low-frequency class.
- An archived repository still occupies its name until it is unarchived or
  purged.

## 8. Tests

- `internal/graft/corestore/sqlstore/archive_test.go`: the round trip, frozen
  writes, read-closed cleanup, observation refusal, tombstone and unknown
  repository.
- `internal/graft/corestore/gate/gate_proof_test.go` and `gate_stale_test.go`:
  removal is refused on every node, archived reads work, read-closed reads are
  refused, and the stale cache of a purged repository is moved away.
- `internal/graft/lifecycle/guard_test.go`: the guard refuses only
  `RemoveRepository`.
- `internal/gitaly/service/storagecore/archive_test.go`: the RPCs, admin-only
  cleanup and purge, observation and state.
- `cmd/graft/storage_test.go`: the CLI lifecycle and explicit admin token.
- `internal/graft/e2e`: a purged repository's objects are collected after the
  object expiration, and another node's cached copy is stale.
