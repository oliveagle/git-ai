# Testing

How this project is tested, from the fastest test to the slowest, and what a
change has to bring with it. The suite is not one pile of tests: it is layers
that answer different questions, and a change is tested at the lowest layer
that can decide its correctness.

## The layers

| Layer | Question it answers | Where it lives | How to run it |
| --- | --- | --- | --- |
| Unit | Does this decision, parser or rule behave as specified, without a server and without Git? | the pure packages of `internal/graft/...` (`branchgate`, `review/rules`, `changelog`, `merge3`, ...), `internal/gitaly/config` | `go test ./internal/graft/branchgate/...` |
| Component | Does this service of the server work with the real Git, the real database and the real storage? | `internal/gitaly/service/...`, `internal/graft/corestore/...` | `go test ./internal/gitaly/service/...` |
| Integration (end to end) | Does the server behave as a client sees it, with real Git clients, the SDK and the CLI? | `internal/graft/e2e/`, `sdk/`, `cmd/graft/` | `go test ./internal/graft/e2e/...` |
| Conformance | Does the same script behave the same on every backend and in every mode? | `internal/graft/conformance/` | `make test-conformance` |
| Compatibility | Does the same suite pass on another Git, another object format, another topology? | the Makefile targets `test-with-*` | `make test-with-reftable` |
| Fault injection | What happens when a process dies, a GC runs, a writer moves a branch under the lands? | `internal/graft/e2e/{chaos,storage_crash,storage_gc,storage_multinode}_test.go` | `go test -run 'TestChaos|TestStorage' ./internal/graft/e2e/` |
| Fuzzing | Does a parser of untrusted input crash, hang or accept garbage? | the parsers of the hook input, the changeset log, the resolver protocol (see [FUZZING.md](FUZZING.md)) | `go test -fuzz=FuzzParseUpdates ./internal/graft/changelog/` |
| Load | Does the server hold its latency and its invariants with many agents on one branch? | `internal/graft/e2e/loadscenario_test.go`, [BENCH.md](BENCH.md) | `make bench` |

The conformance harness is the contract layer: it runs one table of client
steps against `git http-backend` (the reference) and against the server in
every mode it has (standalone, with a storage core in shadow mode and in
authoritative mode, on every database it supports), and compares what every
backend observed. A change of the protocol that a client sees belongs in that
table, not only in a unit test of the parser.

## The rules of a test

- A test decides one thing and names it. The name says what is broken when it
  fails, not what function was called.
- The decision is pure: the helpers that classify outcomes are tested with
  fakes and need neither a server nor Git (see `loadscenario_test.go`).
- No sleeps and no wall-clock assumptions. Time is injected, seeds are fixed,
  and a failing run prints the seed that replays it (`GRAFT_CHAOS_SEED`).
- A test that needs a server starts a real one, with the real configuration
  file, and cleans up after itself. A test that mocks the server is a unit
  test, and says so.
- The tree a test works on is complete. A sparse checkout hides the files a
  change touches, so the integration tests materialize the whole tree
  (`graft clone` without `--sparse`, and the full merged trees the server
  resolves).
- A test that shells out to a build passes `-buildvcs=false`. The harness sets
  `GIT_DIR` to `/dev/null`, to catch the Git commands that run in the wrong
  repository, and the VCS stamping of a build fails on it.

## What a change has to bring

| Change | Tests it has to bring |
| --- | --- |
| A rule, a policy, a decision | a unit test of the decision, table-driven, with the allowed case and the refused cases |
| A service, an RPC | a component test of the service and an integration test through the SDK |
| A CLI command | a test of the command line: the exit status, the JSON output, the error message and the misuse (`cmd/graft/gate_test.go`, `cmd/graft/green_test.go`) |
| A hook, a reference update | an integration test that pushes, lands or deletes against a real server and reads the outcome |
| The wire format, the client table | a row of the conformance table |
| A crash path, a retry | a fault injection test that kills the process or the connection |
| A reload of a policy | a unit test of the store that holds it, and an end-to-end test that signals a real process and reads the change in what the server decides |

## What a local run needs

`make test` reads its environment the way CI does, and three of the
prerequisites fail the run rather than skip when they are missing:

- **A PostgreSQL 16.** The `praefect` and `datastore` tests connect through
  libpq's variables (`PGHOST`, `PGPORT`, `PGUSER`) and fail without them; the
  storage core runs its suites on SQLite and, when `GRAFT_TEST_POSTGRES_DSN`
  is set, again on PostgreSQL. CI starts a `postgres:16-alpine` service with
  trust authentication; a container with `POSTGRES_HOST_AUTH_METHOD=trust`
  and a published port is enough.
- **Git 2.45 or newer in `PATH`.** The conformance harness runs the machine's
  git as the reference client and compares it with the server's bundled git,
  and the authoritative storage modes serve reftable repositories, which older
  git cannot read (`unknown repository extension: refstorage`). The bundled
  versions are built under `_build/deps/git-*`; use one of them when the
  distribution's git is older, or the comparison measures a version
  difference instead of the server.
- **Go 1.26.** The Makefile exports `GOTOOLCHAIN=local`, so the `go` in `PATH`
  has to be the version `go.mod` names, not a toolchain it would download.
- **A temporary directory that behaves like the tests assume.**
  `TEST_TMP_DIR` (default `/tmp`) has to be on a filesystem that accepts a
  4095-byte symlink target (ext4 does; XFS stops at 1024) and is short enough
  for a Unix socket path (108 bytes): the checkout and runtime-directory tests
  run into both limits.

The whole suite is then `make test`. The reftable variants CI runs are
`make test-with-reftable TEST_PACKAGES="$(go list ./internal/git/... ./internal/gitaly/service/... ./internal/gitaly/storage/... | tr '\n' ' ')"`.

## What CI runs

`.github/workflows/ci.yml` runs:

- on every pull request: the storage core, the configuration, the CLI and the
  SDK, and the storage end-to-end tests (`quick`). It is the gate a change has
  to pass to be merged.
- on every push to `main`: the whole suite, split into five groups that run in
  parallel (`git`, `gitaly-service`, `gitaly-other`, `graft`, `other`), plus
  the conformance harness alone in its own step with its own timeout, plus the
  reftable variants of the storage tests.
- always: the `graft-agents` module with and without cgo, and the build of the
  client on Windows.

The same checks run locally:

```
make test                       # the whole suite (alias of test-go)
make test-conformance           # the conformance harness alone
make race-go                    # the race detector
make bench                      # the benchmarks
make verify                     # mod-tidy, NOTICE, proto and lint
```

A single package, or a single test:

```
TEST_PACKAGES=./internal/graft/branchgate TEST_OPTIONS="-count=1 -run=TestDecide" make test
```

## What CI does not run yet

The targets below exist, but no workflow runs them. Locally, the bundled-Git
matrix is green: `test-with-git-master` and `test-with-git-prev` pass the whole
suite, as does `test-conformance` on its own. Two of the variants do not pass
yet, and for suite reasons rather than environment ones:

- `test-with-sha256`: the server-side repositories are SHA-256, but much of the
  suite still creates the client-side ones with a plain `git init`, so the first
  push fails on the format mismatch (`unsupported object format 'sha1'`).
- `test-with-praefect`: the proxy topology fails in its setup, before the
  tested behaviour is reached (node authentication, repository creation).

They are the next things to wire once they pass, in this order:

1. `make test-with-git-master`, `make test-with-git-prev` and
   `make test-with-sha256`: the compatibility matrix. The suite must pass on
   the Git versions the server supports, not only on the bundled one.
2. `make test-with-praefect`: the topology where GitLab talks to a proxy that
   fans out over several nodes.
3. `make race-go`: the race detector over the concurrent paths (the lands, the
   storage core, the land lanes).
4. A scheduled job for the fault injection and load scenarios, which are slow
   and flaky by nature and do not belong to the merge gate.
5. A coverage report: the Makefile writes one with `TEST_WITH_COVERAGE=1`, but
   no workflow collects it and no threshold is enforced.
