# Upgrade the client

The git-ai client is a set of four tools: `graft`, `graft-cache`,
`graft-verify` and `graft-bench`. Releases are published on the GitHub
releases of the project, one package per operating system and architecture,
with a `checksums.txt` that carries the SHA-256 of every package. One command
moves an installed client to a release:

```shell
graft upgrade
```

The command asks the release API for the newest release, downloads the package
of the running platform, checks the SHA-256 of the download against the
checksums file of that release, and replaces the four tools that sit next to
the running `graft`. Each replacement is atomic: the new file is written into
the directory of the running tool and renamed over it, so a build or a hook
that starts while the upgrade runs sees either the old or the new tool, never a
half-written one.

The gitaly server is not part of a release, because it embeds a full build of
Git; upgrade it by building it from source
([doc/development.md](../development.md)).

## The version the server requires

The tools are released apart from the server, so a client and a server may be
different versions. The server can name the oldest client it answers:

```toml
[graft]
min_client_version = "0.9.0"
```

Every client of this project reports its version on every request, in the
`graft-client-version` metadata key. The external API refuses a client below
the minimum with `FailedPrecondition`:

```text
the server requires client version 0.9.0 or newer; this client is 0.8.0:
run `graft upgrade` to update the graft tools, or install the package of this release
```

The refusal is a compatibility answer, not a security control: authentication
still decides who may call what. Three cases are deliberate:

- A client that reports **no** version predates the key, so it is older than
  every release that has it, and is refused while a minimum is set.
- A build made from a checkout reports what that build was told to report, not
  a release version: `make` passes the `VERSION` file through `-ldflags`, so a
  build of this tree reports the Gitaly line (`19.5.0-rc3-g1a2b3c4`), which
  sorts above every graft release; a plain `go build` reports `unknown`. Neither
  can be compared with a release in a way that means anything, and refusing
  them would refuse the server's own tools, so they are answered and the
  version is logged. A deployment that wants a hard floor should therefore set
  `min_client_version` and install released clients, not source builds.
- `ServerInfo` and the gRPC health probes are always answered, so an old
  client can be told what it needs and an orchestrator can still see the
  process is alive.

The gate is enforced on the external listeners only; the internal socket
carries the server's own hook calls, whose version is the server's. With
`min_client_version` empty, every client is answered, which is the default.

## What it does not do

`graft upgrade` replaces the tools of the directory the running `graft` was
started from. A client that was installed with a package manager keeps being
managed by that package manager, and `go install` users keep their `go
install`; run the command from the installation you want to move.

A build between releases carries the describe suffix of its commit, such as
`0.7.0-2-g1a2b3c4`. That build is newer than the release it counts away from,
so upgrade declines to replace it with that release. `--version V` names a
release instead of the newest one.

## Checking without installing

```shell
graft upgrade --check
```

prints the version the running tools were built as and the newest release, and
installs nothing. `graft version` prints the version alone.

## JSON output and scripting

`--json` prints one object with the version the tools had, the release they are
on now, the directory that was touched and the tools that were replaced; the
same object with `"up_to_date": true` answers a `--check` that found the
client current. The exit status is 0 when the client is on the release the run
asked for, 1 when a step failed — a checksum that does not match, a release
that does not carry the package of the running platform — and 2 when the
command line is wrong.

`GITHUB_TOKEN`, when set, is sent to the release API, which raises the rate
limit that an anonymous client has. `--repo OWNER/NAME` points the command at
another repository, for a fork that publishes its own releases.
