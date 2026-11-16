# Graft configuration reference

Graft settings live in the `[graft]` section of Gitaly's TOML configuration
file, next to the usual Gitaly keys. The section is defined in
`internal/gitaly/config/config_graft.go`. Every key is optional. An empty
section keeps upstream Gitaly behaviour, except that the defaults below apply.

The annotated example is [`examples/graft/config.toml`](../../examples/graft/config.toml).
The end-to-end tests in `internal/graft/e2e` load that file and validate it, so
the example stays a working configuration.

## Gitaly keys that standalone mode uses

| Key | Notes |
|---|---|
| `socket_path` | Unix socket to serve on, for example `/var/lib/graft/run/gitaly.sock`. Use `listen_addr` for TCP. Clients address a unix socket as `unix:<path>`. |
| `bin_dir` | Directory with `gitaly-hooks` and `gitaly-ssh`. Required, as in upstream Gitaly. |
| `runtime_dir` | Directory for runtime files. |
| `[auth] token` | Shared token. It is also what the test helpers use for setup calls when agents are configured. |
| `[[storage]]` | Repository storage. Repositories are found below `path` by their relative path. |
| `[gitlab]`, `[gitlab-shell]` | Not required when `[graft] standalone = true`. |

## `[graft]`

| Key | Type | Default | Description |
|---|---|---|---|
| `standalone` | bool | `false` | Run without GitLab. No GitLab secret or URL is required, GitLab is never contacted, and access checks and hook callbacks are answered by the standalone client from `[[graft.policy]]`. Request defaults are applied on the external server. |
| `policy` | array of tables | empty | Ordered access rules, see below. |
| `branch` | array of tables | empty | Branch management rules, see below and [BRANCHES.md](BRANCHES.md). |
| `agent` | array of tables | empty | Per-agent credentials, see below. With at least one agent, every request must present an agent token or the shared `auth.token`. |
| `agent_rpcs` | array of strings | empty | Extra full gRPC method names (for example `/gitaly.RepositoryService/RepositorySize`) that agents may call on top of the built-in allowlist. Startup fails on an unregistered method or on a method that agents may never call. See [AGENT-SECURITY.md](AGENT-SECURITY.md). |
| `land` | table | | `[graft.land.batch]`: batching of concurrent lands, and `[graft.land.resolver]`: the conflict resolver of `UserLand`, see below. |
| `storage` | table | | `[graft.storage]`: the storage core (database plus object storage), see below. |
| `mirror` | table | | `[graft.mirror]`: the mirror policy, see below and [MIRROR.md](MIRROR.md). |

### Request defaults (standalone mode)

On the external server, the request defaults fill the GitLab-specific identity
fields of every request, unary or streaming (each received message of a stream
is handled).

- Every `gitaly.User` that the request contains, at any depth (for example the
  user in the header of the first `UserCommitFiles` message, or a user inside a
  oneof), gets `gl_id` and `gl_username` set to the caller's identity name, or
  to `anonymous` when there is no identity. A request without a `gitaly.User` is
  not given one, so the handler rejects it as it does without Graft.
- Every string field named `gl_id` or `gl_username`, at any depth, gets the same
  value.
- The top-level `repository` field gets `gl_repository` set to
  `graft:<storage>/<relative path>` when it is empty. This value is never
  overwritten, and no other repository field is touched.

For a request made with an agent token, the identity fields are always set to
the agent name, even when the client sent another value, so an agent cannot
claim another actor's user ID. For the shared token and for unauthenticated
requests, they are filled only when empty and a value the client sent is kept.
The user's name and email, which become the committer of new commits, are never
changed. See [LIMITATIONS.md](LIMITATIONS.md), Security limitations.

### `[[graft.policy]]`

The policy answers the access check for a reference update. For each changed
reference, the first rule that applies decides:

- A rule applies when its `actor` matches the actor of the request and one of
  its `refs` globs matches the reference. The actor is the agent name for a
  request made with an agent token. Otherwise (shared token, no
  authentication) it is the acting user ID of the request (`gl_id`), see
  [LIMITATIONS.md](LIMITATIONS.md). An empty `actor` or `"*"` matches every
  caller. Empty `refs` match every reference.
- A reference that no rule applies to is denied. This includes tags and any other
  reference that the rules do not name. Add a final rule with `allow = true` to
  open everything that is left.
- An update is allowed only if every changed reference is allowed. The denial
  message names the rule that decided, or says that no rule matched.
- When the policy is empty, every update is allowed.
- For requests made with an agent token, the policy holds for every reference
  update under `refs/`, whichever RPC performs it (for example `WriteRef` and
  `DeleteRefs`, not only `UserLand` and pushes). See
  [LIMITATIONS.md](LIMITATIONS.md), "Which updates the policy covers", which
  also lists the administrative RPCs that agent tokens cannot call.
- A batched land is decided per request, with the actor of each request, before
  the request joins the batch. The combined update of the batch, made as the
  synthetic user `graft-batch`, is not checked again, so a rule for
  `graft-batch` has no effect. See
  [LAND-BATCHING.md](LAND-BATCHING.md#hooks-and-identity-of-the-combined-update).

| Key | Type | Default | Description |
|---|---|---|---|
| `actor` | string | `""` (everybody) | Glob (`path.Match` syntax) matched against the actor of the request: the agent of the token, or the acting user ID for the shared token. |
| `refs` | array of strings | `[]` (every reference) | Globs matched against fully qualified references such as `refs/heads/main`. Syntax is `path.Match`, plus a trailing `/**` that matches any depth, so `refs/heads/**` matches `refs/heads/feature/x`, and any other trailing `**` that matches any remainder, so `refs/heads/release**` matches `refs/heads/release/1.0`. Invalid globs and empty entries are rejected at startup, and by a reload, which keeps the policy in force. |
| `allow` | bool | `false` | Whether a matching update is allowed. |

Example: protect `main` for everybody except the `planner` agent, and allow
everything else:

```toml
[[graft.policy]]
actor = "planner"
refs = ["refs/heads/main"]
allow = true

[[graft.policy]]
refs = ["refs/heads/main"]
allow = false

[[graft.policy]]
allow = true
```

#### Reloading the policy

Send SIGUSR1 to the gitaly process (or to gitaly-wrapper, which forwards it):
it reads its configuration file again and puts the new `[[graft.policy]]` in
force, and the new `[[graft.branch]]` rules with it. SIGHUP is not a reload: it
stops the server, or upgrades it in place with the graceful-upgrade bootstrap.

- Only the access policy and the branch rules are applied. Other changes of the
  file, `[[graft.agent]]` and `[graft.review]` included, are logged at WARN as
  needing a restart, and not applied.
- A file that cannot be read, or whose policy is invalid (a bad glob, an empty
  `refs` entry, a branch rule without a name or two rules for one branch), is
  logged at ERROR and changes nothing: the policies in force stay, so a typo
  never opens a managed branch. Actors of the new policy that name no
  configured agent are logged at WARN; they are not an error (an actor can be
  the user ID of a caller with the shared token).
- An update in flight is decided by the policy in force when the
  reference-transaction hook runs, while git holds the references' locks: a
  tightening applies to it even if its earlier checks allowed it. A land batch
  whose members were admitted under another policy is refused there, and its
  requests land alone, each decided by the policy in force. A batch attempt
  judges each of its members with the policy in force when the attempt
  started: a loosening reaches the members of the next attempt.
- Each node reloads its own file: reload every node, then compare their
  `graft_policy_info`.

Metrics: `graft_policy_reloads_total{result}` (`applied`, `unchanged`,
`rejected`), `graft_policy_generation` (1 at startup, one more for every
applied reload) and `graft_policy_info{sha256}` (1 for the hash of the
policy in force). A denied agent update is logged at INFO with the
`policy_generation` and `policy_sha256` that denied it; an allowed one at
DEBUG.

The branch rules have their own series: `graft_branch_policy_reloads_total{result}`
(`applied`, `unchanged`, `rejected`) and `graft_branch_policy_generation` (1 at
startup, one more for every reload that changed the rules). A reload that
changed them is logged at INFO as `branch policy reloaded`, with the generation
and the number of rules.

Before a reload, `graft policy check FILE` validates the file's policy as the
reload will (exit 2 when the policy is invalid) and prints its hash; with
`-repo REPO` it lists the recent updates of the repository that the new policy
would deny, and exits 1 when there are any. It checks the policy only: the reload also validates the rest of
the file, against the server's own paths and settings, and rejects a file that
fails there.

### `[[graft.branch]]`

The branch management policy: what may update a branch, and what a change must
carry to land on it. A branch without a rule is not managed. The rules are
decided by the reference-transaction hook, in its prepared phase, so that a
refusal aborts the update. See [BRANCHES.md](BRANCHES.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | required | The branch the rule governs, without the `refs/heads/` prefix. Two rules for one branch are refused when the configuration is loaded, as is a name that is not a branch name. |
| `require_review` | bool | `false` | A change that lands on the branch must have a review of it, on that branch, approved by the approval rules of `[graft.review]`. |
| `require_approvals` | int | `0` | The number of current approvals the review must carry, on top of the approvals the approval rules ask for. |
| `allow_stale_base` | bool | `false` | Accept a review whose base is not the current tip of the branch. Without it a change that is based on an older tip of the branch does not land. |
| `allow_direct_update` | bool | `false` | Accept an update of the branch that is not a land, such as a push. Without it only a land may update the branch. |
| `require_verified` | bool | `false` | The verification checkpoint of the branch must cover it: the checkpoint (`refs/graft/green/<branch>`, moved by graft-verify) must point at the tip the land builds on. |
| `require_verified_result` | bool | `false` | The checkpoint must point at the commit the update produces, not at the tip it builds on: the branch takes a land only after the result of that very land was tested, which is the rule of a merge queue. It implies `require_verified`. A caller that does not name the result is refused with `result_unknown`; the hook always names it, and `graft gate check` names it with `--result`. A result the branch would no longer take, because the branch moved after it was computed, is refused with `stale_result`. |
| `require_full_tree` | bool | `false` | The checkpoint must certify the whole tree of the commit it names: the tree the suite ran on (`refs/graft/green-tree/<branch>`) is the tree of that commit. A checkpoint that certifies no tree, or only part of one, is refused with `not_full_tree`. A suite that ran on a sparse checkout says nothing about the files it could not see, so a branch that asks this rule takes no land whose tree was not tested whole. |
| `require_suites` | list of strings | empty | The suites the checkpoint must certify: the publisher names them with `graft green set --suite`, each in a reference of its own (`refs/graft/green-suite/<suite>/<branch>`), and a checkpoint that certifies only some of them is refused with `suites_missing`. A suite name is a reference component without a slash, such as `unit` or `integration`. It is the rule of a project whose change needs its unit suite and its integration suite. |

The gate binds the callers that authenticated with a per-agent token, like the
access policy does. The operator credential and the server's own operations are
trusted, so a CI that lands with the shared token asks the same question with
`graft gate check --branch B`, which the server answers with these rules and
with the state of the branch (exit 0 when the change may land, 1 when the
policy refuses it, 2 when the command line is wrong). `graft gate rules` prints
the rules the server enforces.

### `[graft.mirror]`

The mirror policy: a repository is published to a mirror before it takes a
push, and a mirror that falls behind holds the pushes back. The policy is
decided by the reference-transaction hook, in its prepared phase, with the
state the `MirrorService` holds: the targets of the repository, and the commit
every mirror holds for every branch, recorded by `graft mirror push
--mirror-target NAME` and by every `graft mirror queue run`. See
[MIRROR.md](MIRROR.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `required` | bool | `false` | A repository that declares no mirror target, or whose mirrors never published successfully, takes no push. A push to a branch that no target publishes is a warning, not a refusal. |
| `warn_lag` | int | `10` | A mirror this many commits behind the pushed branch warns the pusher, and the push goes through. A negative value turns the warning off; the warning is written to the stderr of the push. |
| `deny_lag` | int | `0` (never) | A mirror this many commits behind refuses the push. It must be greater than `warn_lag`, so that a push is warned about before it is refused, and a server whose configuration refuses that at load time does not start. |

The policy is off unless one of the three keys is set: a server that sets
none of them takes every push. The refusals are the codes of
`graft mirror status`, which asks the same question the hook decides and exits
1 when a push would be refused.

### `[[graft.agent]]`

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | required | Identity of the agent. It is the identity the token maps to, the actor the policy decides on for the token's requests, the actor that the changeset log records for the token's updates and, in standalone mode, the user ID (`gl_id`, `gl_username`) of every request made with the token, whatever the client sent. The name `shared` is reserved for the shared token, and `proposals` and `green` for the server's references under `refs/graft/`: the workspace's temporary branches of an agent are `graft/<name>/...`, which for those names an agent may not use. |
| `token` | string | — | The secret. Set either `token` or `token_file`. |
| `token_file` | string | — | Path to a file that holds the secret, surrounding whitespace removed. Read at startup. |

Clients send the token as a plain bearer token in the standard `authorization`
metadata. The v2 HMAC token format, computed with the agent token as the secret,
is accepted too. Comparison is constant time. The shared `auth.token` is accepted
only in the v2 HMAC format, never as a plain bearer token. Health checks never
require a token.

Agent names and tokens must be unique. Startup fails when a name is empty or
reserved, a name repeats, two agents share a token, an agent token equals the
shared `auth.token`, both `token` and `token_file` are set, or a token file is
missing or empty.

### `[graft.object_service]`

| Key | Type | Default | Description |
|---|---|---|---|
| `max_objects` | int | `1000` | Maximum number of object IDs per `GetObjects` request. A larger request fails with `InvalidArgument`. The SDK splits large reads automatically and adapts to a lower limit. |
| `chunk_size` | int | `1048576` (1 MiB) | Maximum data bytes per `GetObjects` message. |

`graft-cache` has its own `--max-objects` and `--chunk-size` flags. Set them to
the same values as the upstream, so that a cache tier returns the same stream.

### `[graft.changelog]`

| Key | Type | Default | Description |
|---|---|---|---|
| `disabled` | bool | `false` | Turn off recording of reference updates in `<git dir>/graft/changesets.jsonl`. New reference updates are not recorded. Entries recorded earlier stay in the log, and the changeset RPCs still return them. |

### `[graft.derived]`

| Key | Type | Default | Description |
|---|---|---|---|
| `disabled` | bool | `false` | Turn off the derived-data store and the commit-graph writes after updates. `Blame`, `PathHistory` and `LastCommitForPath` compute their results from scratch on every call. |
| `eager` | bool | `false` | Compute the `graph` and `changed-paths` kinds in the background right after a reference update commits, so that the first read finds them. Lazy otherwise. Has no effect when `disabled` is true. See [DERIVED.md](DERIVED.md). |
| `workers` | int | `2` | Background workers that compute derived data eagerly. Zero means the default. |
| `queue_size` | int | `10000` | Batches (one per committed transaction) waiting for the workers. When full, a batch is dropped and its commits are computed on first read. Zero means the default. |
| `commit_graph` | bool | `false` | Write the commit-graph incrementally after committed updates (debounced, through housekeeping). Off by default since phase 4a; with `false` no commit-graph is written outside housekeeping. A phase 3 server that relied on the old default must set `true`. Has no effect when `disabled` is true. See [DERIVED.md](DERIVED.md#commit-graph-writes). |

### `[graft.land]`

| Key | Type | Default | Description |
|---|---|---|---|
| `graph_ancestry` | bool | `false` | Answer the history questions of `UserLand` from the stored `graph` index (see [DERIVED.md](DERIVED.md)) instead of running `git`, for every land, batched or not: the listing of the commits that a request adds to the branch, and the ancestry and merge-base checks that make that listing exact when committer dates are skewed (see [LAND-BATCHING.md](LAND-BATCHING.md#how-a-request-is-decided-and-rebased)). The index lists the exact commits in one walk; both ways return the same answers. If the index cannot be built, or the repository has shallow commits, grafts or replace refs, the server falls back to `git` and logs it. With `[graft.derived] disabled = true` the index is computed in memory for each landing attempt and nothing is written under `graft/derived/`. |

### `[graft.land.batch]`

Batching of concurrent `UserLand` requests on one branch: requests are queued per
branch, and a batch is published with one reference update. Standalone mode only.
See [LAND-BATCHING.md](LAND-BATCHING.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `disabled` | bool | `false` | Turn batching off. Every request then runs its own compare-and-swap loop, as in phase 2. |
| `window` | string (Go duration) | `"10ms"` | How long a lane waits after its first queued request before it runs the batch. `"0s"` means no wait: a batch is whatever queued while the previous batch ran. Must not be negative. |
| `max_size` | int | `50` | Largest number of requests in one batch, at most `100`: the update of a batch records the identity of every member ([PLAN11](PLAN11.md)) in the environment of its git processes. Zero means the default; negative values and values above `100` are rejected. |
| `target_latency` | string (Go duration) | `"500ms"` | Batch duration the lane aims for. A lane halves its effective maximum batch size after a batch slower than this, and grows it back (up to `max_size`) after batches faster than half of it. `"0s"` disables the adaptation, and the lane always uses `max_size`. Must be a valid, non-negative Go duration. See [LAND-BATCHING.md](LAND-BATCHING.md#adaptive-sizing). |

### `[graft.land.resolver]`

Automatic conflict resolution during `UserLand`. Off by default. The server
never calls a language model: it calls the program or HTTP endpoint configured
here. The protocol, the validation rules and `graft-resolve` are described in
[RESOLVE.md](RESOLVE.md).

A resolver configuration the server could not use is a startup error: an empty
`command`, a `url` that is not an `http` or `https` URL with a host, a
`token_file` that cannot be read or is empty, a bad `timeout`, a negative
bound, or a `max_file_bytes` over the bound of a check request. A configured
resolver is never treated as no resolver: if it cannot be built later (its
token file removed, say), every land that needs it fails closed, with the
cause.

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Master switch. When false, `resolve_conflicts` in a land request is ignored. |
| `command` | array of strings | empty | Program and arguments. It reads one request on stdin and writes the answer on stdout. |
| `url` | string | empty | `http://` or `https://` endpoint that receives the request as a JSON POST. Exactly one of `command` and `url` must be set when `enabled` is true. |
| `token_file` | string | empty | For `url`: file with the bearer token, read at startup. |
| `timeout` | string | `"120s"` | Bound on one resolver call (Go duration), and on the whole land that the server resolves for `resolve_conflicts`: its rounds (the resolutions, the verifications of a model's resolution, the waits for slots, and the passes through the branch's lane) end at this deadline, which is `timeout` plus `verify.timeout` when `verify.enabled`. A land that is still unresolved at this deadline does not fail with a deadline: it ends with the merge conflict and its package, the proposals of what is known and the files that changed under it as not attempted, with the reason. |
| `max_files` | int | `20` | Conflicted files per land. A land with more is not sent to the resolver. |
| `max_file_bytes` | int | `1048576` (1 MiB) | Bound on every side of every file sent and on every resolved file accepted. At most 12566528 (about 12 MiB): one file, its three sides and its resolution, must fit one check request. |
| `proposal_ttl` | string | `"168h"` | How long the proposals of a failed land are kept under `refs/graft/proposals/` (Go duration, by the server's clock); at most 20 are kept per branch. Lands with a resolution and the housekeeping pass prune them. |
| `check_concurrency` | int | `2` | Checks of agents' resolutions that run at once in the Gitaly process, across all its listeners (one resolver client is shared by them); each holds a request of up to 64 MiB. A check waits for a free slot within its deadline (`timeout`), and fails closed when it expires. The wait is in `graft_resolver_check_slot_wait_seconds`. |
| `verify_per_repository` | int | `1` | Verifications that run at once for one repository (of any resolution: a rule's, an agent's, a model's), inside `verify_concurrency`: every land of a busy branch builds a tree of its own to verify, and one repository must not take the whole pool. A land waits for its repository's turn first, holding nothing of the pool, within the land's deadline. **The cost, stated:** a resolution lands only on the tree that was verified, so a land whose branch moved after its tree was verified is verified again, and on a busy branch the lands take their turns one repository at a time; at the deadline a land ends with the package (a rule's files `NOT_ATTEMPTED` with the reason, a model's `PROPOSED`) and the agent lands again. Lands of rules are batched only when `verify.enabled` is not set. |
| `verify_concurrency` | int | `2` | Verifications that run at once in the Gitaly process (of any resolution: a rule's, an agent's, a model's), all out of the lane, in a pool of their own that exists whenever `verify.enabled` is set, with or without an AI resolver, as `verify.command` takes minutes. A verification waits for a free slot within the land's deadline. The wait is in `graft_resolver_verify_slot_wait_seconds`. |
| `resolve_concurrency` | int | `2` | Resolutions that run at once in the Gitaly process, in a pool apart from the checks', so that long resolutions never delay a check. A resolution waits for a free slot within the land's deadline (`timeout`), and fails closed when it expires. The wait is in `graft_resolver_resolve_slot_wait_seconds`. The lane's inline calls (`inline_timeout`) have a pool of their own, of at least 8 slots, which the lane never waits for. |
| `resolve_rounds` | int | `32` | Safety cap on the rounds of a land whose conflicting files keep changing while it is resolved out of the lane. The rounds are bounded by `timeout`; a land that loses a round waits for its turn on its files in a queue, so that it does not lose them one after the other (RESOLVE.md, "Out of the lane"), and the cap is not reached in practice. A round that enters the lane without a pre-resolution (the first, and the ones after a wait for a turn; at most 4) does not count. It is no contract. |
| `inline_timeout` | string (Go duration) | `"1s"` | Bound of the call with which the lane resolves a member's conflicting files itself, as the rules (trivial, union, rerere, a command that is a rule) and never a model: the request carries `rules_only` and announces no capability but `origin`. The lane never waits for a slot (the calls have a pool of their own; with it taken the member goes out of the lane), and the time it spends in inline calls is bounded once per batch attempt: all the time of every call, a successful one too, is charged to the batch, and once the batch has spent `inline_timeout` the other members go out of the lane without a call (fast rules, milliseconds, do not notice; a resolver that leaves child processes holding its output may keep the lane up to 2 s more, the kill delay). A member whose files the rules cannot resolve within it is resolved out of the lane, as any other. `"0s"` turns the inline resolution off; a negative or invalid value is a configuration error. Never used with `verify.enabled`: every tree is then verified out of the lane. |
| `paths_allow` | array of strings | empty (every path) | Globs of paths that may be sent to the resolver, with the syntax of policy refs. |
| `paths_deny` | array of strings | empty | Globs of paths that are never sent. Deny wins over allow. |
| `actors` | array of strings | empty (every caller that may land) | Globs of the identities that may request resolution: agent names, or `shared` for the shared token. Without `[[graft.agent]]` no caller has an identity, so a non-empty list admits nobody (the server warns at startup). |
| `lockfiles` | array of strings | empty | Base names of lockfiles, besides the built-in `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `poetry.lock`, `Cargo.lock` and `go.sum`. A conflicting lockfile is never sent to the resolver: it goes to the agent. |
| `generated_markers` | array of strings | empty | Go regular expressions, besides the built-in `Code generated .* DO NOT EDIT\.`, `@generated`, `DO NOT EDIT` and `(?i)auto-generated`, searched in the first 10 lines of every side. A match marks the file generated, and it is never sent. A path that the tip's `.gitattributes` marks `linguist-generated` is generated as well. |
| `minified.globs` | array of strings | empty | Base-name globs of minified files, besides the built-in `*.min.js`, `*.min.mjs`, `*.min.css` and `*.bundle.js`. Such files are never sent. |
| `minified.average_line_bytes` | int | `300` | A side larger than 2 KiB whose average line is longer is minified. |
| `minified.longest_line_bytes` | int | `5000` | A side larger than 2 KiB with a longer line is minified. |
| `verify.enabled` | bool | `false` | Run `verify.command` on the merged tree of every resolution (a resolver's or an agent's) before it lands; it applies even when `enabled` is false. It is also what lets a model's resolution land: only a tree it passed on lands, and without it a model's resolution is only ever a proposal. |
| `verify.command` | array of strings | empty | Program and arguments, run in a temporary checkout of the merged tree with `PATH`, `HOME` and `TMPDIR` only. A non-zero exit rejects the resolution. |
| `verify.timeout` | string | `"5m"` | Bound on one verification, the checkout of the tree and the command together (Go duration); one that does not finish rejects the resolution. When `verify.enabled`, it is added to `timeout` for the deadline of a resolved land. |
| `verify.max_tree_entries` | int | `200000` | Files, links and submodules in the tree that is checked out for the command, plus the directories made for them; a bigger tree is not verified, and the package says why. |
| `verify.max_tree_bytes` | int | `2147483648` | Bytes of the files of that tree and of the targets of its links, checked per entry from the listing, before that entry's content is read. |
| `verify.max_tree_depth` | int | `64` | Components of a path of that tree. A tree can name one deep subtree many times; the expansion is what this and the entry count bound. |

A file that is never sent goes to the agent with its reason in the conflict
package: large (a side over `max_file_bytes`), binary (a NUL byte), lockfile,
generated or minified, checked in that order (see [RESOLVE.md](RESOLVE.md)).

Startup fails when both or neither of `command` and `url` are set, the URL has
another scheme, `timeout` is not a duration, a bound or a threshold is
negative, a glob is empty or invalid, a lockfile is not a base name, or a
