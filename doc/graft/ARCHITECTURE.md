# Graft architecture

Graft is a fork of Gitaly that turns it into a standalone, agent-scale,
server-centric version control server. This document describes how the pieces
fit together and maps each idea from Anthropic's description of its internal
"Scion" system to the Graft component that implements it.

Everything described here is implemented. Items that are not implemented are
marked "not yet" and are listed in [LIMITATIONS.md](LIMITATIONS.md).

## Components at a glance

```
                  agents, CI jobs, sparse workspaces, scripts
        +----------------------------------------------------------------+
        |  graft CLI (cmd/graft)          Go SDK (sdk/)                  |
        +--------------------------------+-------------------------------+
                                         |  gRPC (unix socket or TCP)
                                         v
        +----------------------------------------------------------------+
        |  graft-cache, daemon tier (cmd/graft-cache, --tier daemon)     |
        |  - byte-bounded object LRU in memory                           |
        |  - coalesces concurrent misses, verifies object IDs            |
        +--------------------------------+-------------------------------+
                                         |  gRPC, one batched GetObjects per miss set
                                         v
        +----------------------------------------------------------------+
        |  graft-cache, cache tier (regional, optional, --tier cache)    |
        |  - LRU plus optional on-disk store (--disk-dir)                |
        +--------------------------------+-------------------------------+
                                         |  gRPC, transparent proxy
                                         v
   +-----------------------------------------------------------------------------+
   |  gitaly  ([graft] standalone = true, no GitLab)                             |
   |                                                                             |
   |  auth: SSH-signed agents, the shared auth.token, the admin token            |
   |  requestdefaults: sets gl_id / gl_username from the authenticated identity  |
   |                                                                             |
   |  ObjectService      ChangesetService    DerivedDataService   OperationService|
   |  GetObjects         ListChangesets      Blame                UserLand        |
   |  GetTree            WatchChangesets     PathHistory          (and all others)|
   |        |                  |                   |                    |         |
   |        v                  v                   v                    v         |
   |  catfile cache      changelog store     derived store        quarantine,    |
   |  (object reads)     graft/changesets    graft/derived        compare-and-   |
   |                     .jsonl (flock)      content-addressed    swap ref update|
   |                                                                             |
   |  standalone client: [[graft.policy]] answers the access checks that GitLab  |
   |  would answer (update allowed, pre-receive, post-receive)                   |
   +----------------------------------------+------------------------------------+
                                            |
                                            v
                    bare Git repositories (normal Git, on disk)
```

Every repository stays a normal Git repository. Anything that speaks upstream
Gitaly RPCs keeps working. Graft adds RPCs, and it changes how access checks are
answered when GitLab is absent.

## Mapping of Scion ideas to Graft

The article describes a handful of ideas. The table lists each one and the
Graft component that implements it.

| Scion idea (from the article) | What it means | Graft component | Where |
|---|---|---|---|
| Always-online clients | Clients keep no full clone; they ask the server for what they need, when they need it | `ObjectService` (`GetObjects`, `GetTree`), `graft clone` and `graft sparse`, the SDK | `proto/object.proto`, `internal/gitaly/service/object`, `cmd/graft`, `sdk` |
| Fetch exactly what is needed | Sparse trees and single blobs instead of packfile negotiation | `GetTree` with `recursive` and `include_sizes`; `GetObjects` by OID, resumable after a dropped stream | `internal/gitaly/service/object` |
| Source control as an API | History operations (blame, path history) run on the server | `DerivedDataService` (`Blame`, `PathHistory`) and the existing `CommitService` RPCs | `internal/gitaly/service/derived`, `internal/graft/derived` |
| Landing without a merge queue | The server rebases or merges a change onto the current tip and retries when the tip moves | `OperationService.UserLand` with compare-and-swap retries (`max_attempts`, default 20) | `internal/gitaly/service/operations/land.go` |
| Changeset log | Every accepted reference update is recorded in an ordered log with a dense sequence that clients can follow | `ChangesetService` (`ListChangesets`, `WatchChangesets`) over `<git dir>/graft/changesets.jsonl` | `internal/graft/changelog`, `internal/gitaly/service/changeset` |
| Derived data computed once | Expensive answers (last commit that changed a path, blame) are memoized under immutable keys and reused | Content-addressed derived store keyed by SHA-256 of (kind, commit, path) | `internal/graft/derived`, `internal/gitaly/service/derived` |
| Strict cache tiers | Immutable objects are cached at several layers, and every stored object is verified | `graft-cache` in daemon and cache tiers; byte-bounded LRU, optional disk store, one batched upstream call per miss set | `cmd/graft-cache`, `internal/graft/cachetier` |
| Access control without a central queue | Who may update which reference is decided by a local, ordered policy, per request | Standalone client evaluating `[[graft.policy]]`; the policy decision and the changeset actor use the agent of the token (see LIMITATIONS.md) | `internal/gitlab/standalone_client.go`, `internal/gitaly/server/auth` |
| Automatic conflict resolution (phase 2) | A land that conflicts is resolved by a tool instead of bouncing back to the author | `UserLand` with `resolve_conflicts` calls an operator-chosen resolver over a versioned JSON protocol; `graft-resolve` ships deterministic strategies and an optional `llm` strategy outside the server ([RESOLVE](RESOLVE.md)) | `internal/gitaly/service/operations/land_resolve.go`, `internal/graft/resolver`, `tools/graft-agents/resolve` |
| Test after landing, revert the culprit (phase 2) | Changes land first; tests run on the tip afterwards, and a breaking change is found by bisection and reverted | `graft-verify`: green ref, bisection, `UserRevert` with a `Graft-Auto-Revert` trailer and a cap per hour ([VERIFY](VERIFY.md)) | `cmd/graft-verify`, `internal/graft/verify` |
| Monitoring agents ("superfans", phase 2) | In the article, one agent per component reads its telemetry and logs to catch subtle problems. Graft's superfans follow the changeset stream instead (not the servers' telemetry) and report what matters to each component's owners | `graft-superfan` reads `graft watch --json`, applies deterministic rules and sends digests; an optional `llm` summarizer ([SUPERFAN](SUPERFAN.md)) | `tools/graft-agents/superfan`, `tools/graft-agents/notify` |
| Access control for agents (phase 2) | The article mentions repository- and directory-level permissions; Graft gives agents the least authority that lets them land (directory-level permissions are not implemented) | Agent RPC allowlist, policy on every reference update including `HEAD` ([AGENT-SECURITY](AGENT-SECURITY.md)) | `internal/gitaly/server/auth`, `internal/gitaly/hook/referencetransaction.go` |

## Data flows

### Land (`graft commit --land` or `UserLand`)

```
client                      graft-cache (optional)        gitaly (standalone)
  |                                |                              |
  | CommitFiles(start=base,        |                              |
  |   branch=graft/<user>/<rand>)  |  pass-through                |
  |------------------------------->|----------------------------->| write commit C,
  |                                |                              |   update the temporary branch
  |                                |                              |
  | UserLand(branch=main,          |                              |
  |   commit=C, strategy=rebase)   |                              |
  |------------------------------->|----------------------------->| 1. read the tip T of main
  |                                |                              | 2. T is an ancestor of C: fast-forward
  |                                |                              |    C is reachable from T: already_landed
  |                                |                              |    C landed before as C', C' reachable
  |                                |                              |      from T: already_landed (PLAN11)
  |                                |                              |    otherwise rebase C onto T
  |                                |                              | 3. update hooks and the standalone policy
  |                                |                              | 4. compare-and-swap main from T to C'
  |                                |                              |    lost the race: retry from 1 (jittered)
  |                                |                              | 5. changeset: operation=land, actor,
  |                                |                              |    rewritten=true when history changed,
  |                                |                              |    lands=[C -> C']
  |   UserLandResponse             |                              |
  |<-------------------------------|<-----------------------------|
  |                                |                              |
  | UserDeleteBranch(temporary)    |  pass-through                |
  |------------------------------->|----------------------------->| changeset: operation=delete_branch
```

The temporary branch is written with `commit_files` and removed with
`delete_branch`, so the changeset log shows both steps next to the `land`.

### Read (sparse checkout and history)

```
client                 graft-cache daemon              cache tier             gitaly
  | GetTree(rev, path, |                                      |                    |
  |  recursive)        |  no tree_oid: forwarded              |                    |
  |------------------->|------------------------------------->|------------------->| tree listing
  |                    |                                      |                    |
  | GetObjects(oids)   | memory hit: stream from the cache    |                    |
  |------------------->| miss: all misses in ONE call --------|------------------->| catfile batch read
  |                    | verify IDs, store, coalesce          |                    |
  | chunks in order    |                                      |                    |
  |<-------------------|                                      |                    |
  | Blame(rev, path)   | forwarded (derived data is not cached at this tier)       |
  |------------------->|------------------------------------->|------------------->| lastmod(commit, path)
                                                                                  |   memoized in
                                                                                  |   graft/derived;
                                                                                  |   blame cached at
                                                                                  |   the lastmod commit
```

### Watch (changeset log)

```
client                                          gitaly
  | WatchChangesets(after_sequence=N)              |
  |----------------------------------------------->| stream the backlog: sequence > N, in order
  |<-----------------------------------------------| ...
  |                                                | keep the stream open. A new changeset is
  |                                                | sent when the log is appended (in-process
  |                                                | broadcast) and by a re-read every second
  |<-----------------------------------------------| new changesets, dense sequence
  | cancel                                         |
  |----------------------------------------------->| end the stream
```

## Consistency and failure model

- Reference updates go through Gitaly's normal transaction path, so hooks and
  the reference-transaction hook see every change. The changeset log is appended
  from that hook after the update is committed. A failure to record is logged
  and never fails the update.
- The changeset log is append-only and its sequence numbers are dense from 1.
  Appends are serialized per repository with an in-process mutex and an
  `flock` on `changesets.jsonl.lock`, and are fsync'd. A torn last line after a
  crash is ignored and truncated on the next append.
- Objects are immutable, so `GetObjects` responses can be cached by OID. The
  cache recomputes the object ID of every object it stores from the object
  type, size and content, and refuses to serve or store an object that does not
  match.
- Derived data is content-addressed and immutable. Entries are written
  atomically (temporary file and rename). A crash leaves at most a missing entry,
  which is recomputed on the next read.

## Trust boundaries

- A Graft server refuses anonymous callers: every request must carry a
  credential. Agents authenticate with an SSH key pair. The agent holds the
  private key and signs every request; the server holds the public key of each
  agent in `[[graft.agent]]` and verifies the signature before it dispatches
  the call. The agent name is the identity of that key and the only name the
  agent can act under. The shared `auth.token` and `graft.admin_token` are
  operator credentials for tooling and for the super-admin RPCs, not agent
  identities. See [PLAN14.md](PLAN14.md) and
  [AGENT-SECURITY.md](AGENT-SECURITY.md).
- The signature covers the timestamp, a nonce, the method and the SHA-256 of
  the request body, and a request whose signature does not verify, is outside
  the validity window or reuses a nonce is refused. A request that carries a
  signature never falls back to another credential: a bad signature is an
  authentication failure, not an anonymous request.
- In standalone mode, requests signed with an agent key always get the agent
  name as their acting user ID. Requests made with the shared token get the
  identity name (`shared`) only when they carry no acting user ID.
- The acting user ID is a value the client sends in `gitaly.User.gl_id`. For a
  request signed with an agent key, neither the policy nor the changeset actor
  uses it: both use the agent name, carried to the hooks in the hooks payload.
  For the shared token, the policy decides on the acting user ID. See
  LIMITATIONS.md under Security limitations.
- In standalone mode there is no GitLab. The access policy in `[[graft.policy]]`
  is the only authorization check for reference updates. Treat the policy file
  as security configuration. For agent-signed requests, the
  reference-transaction hook enforces it on every reference update. The
  pre-receive hook enforces it on pushes and OperationService RPCs for every
  caller. An agent cannot call the administrative RPCs that bypass reference
  transactions, such as `RemoveRepository`, `SetCustomHooks` and `HookService`.
  See LIMITATIONS.md, "Which updates the policy covers".
- The cache tier forwards the client's credentials upstream when the client
  sent them. A signature covers the caller's method and body, so the tier
  cannot reuse it for its own upstream call: a read signed by an agent is
  served from the cache, and checked against the upstream, only when the tier
  has its own `--token`. Without `--token`, such a read is forwarded upstream
  uncached. Other callers use their own `authorization` metadata, or the tier's
  token when they send none. With `--token` set, every client that can reach
  the tier without credentials gets the tier token's access; keep the tier
  socket private. See the package documentation of `internal/graft/cachetier`.
