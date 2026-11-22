# PLAN14: no anonymous access, and every agent signs its requests

Status: approved. A Graft server serves no anonymous caller. An agent is an
SSH key pair, and every request of an agent carries a signature over that
request, which the server verifies.

Scope: the authentication layer of the Graft server (`internal/gitaly/server/auth`),
the signature protocol (`internal/graft/agentkey`), the SDK and CLI that sign,
and the cache tier that forwards signed requests.

## 1. The problem

Two things were wrong with the shared-secret model:

1. **Anonymous callers were served.** Upstream Gitaly serves a request without a
   token when none is configured, and a request that fails authentication was
   let through in transitioning mode. A Graft server is reachable by scripts
   and agents on the same host and, often, on the network: a missing
   credential must be a refusal, not a fallback.
2. **An agent was identified by a secret it sent.** A per-agent bearer or HMAC
   token identifies the *secret*, not the caller: anyone who reads the token
   file or the process environment is that agent, and a captured request can be
   replayed until the token is rotated. The requirement is an explicit identity
   per agent, authorized by an SSH key pair, verified on every request.

## 2. Credentials

A Graft server is one that runs standalone (`standalone = true`), with the
storage core enabled, or with a credential of its own configured
(`[[graft.agent]]`, `graft.admin_token`/`graft.admin_token_file`): that is
`cfg.Graft.RequiresAuthentication()`. It refuses every request that carries no
credential, and `Cfg.Validate` (and `ValidateV2`) refuses to start one without
`auth.token`: the Git hooks and the operator's tools call the server with it,
so agents and `graft.admin_token` do not replace it. An upstream Gitaly
without a token keeps its behaviour.

| Credential | Configured as | Identity (`identity.Source`) |
|---|---|---|
| agent's SSH key signature | `[[graft.agent]] name` + `public_key`/`public_key_file` | agent name (`SourceAgent`) |
| super admin's secret | `graft.admin_token`/`graft.admin_token_file` | `admin` (`SourceAdmin`) |
| shared token | `auth.token` | `shared` (`SourceSharedToken`) |

The shared token is the operator's credential, and the one the Git hooks and
the operator's tools use. The super admin's secret is the only credential that
may purge an archived repository (doc/graft/PLAN13.md). An agent has no shared
secret at all.

## 3. The signature

An agent holds an SSH key pair. The server holds the public key and looks the
agent up by its SHA-256 fingerprint, the same string
`ssh.FingerprintSHA256` prints (`SHA256:...`).

Every request carries four metadata keys:

| Metadata | Content |
|---|---|
| `graft-key` | `SHA256:...` fingerprint of the public key |
| `graft-timestamp` | Unix time of the request, seconds |
| `graft-nonce` | 16 random bytes, hex |
| `graft-signature` | base64 SSH (RFC 4253) signature of the payload |

The payload is deterministic, and binds the signature to one call:

```
graft-ssh-v1\n
<timestamp>\n
<nonce>\n
<full method>\n
<hex SHA-256 of the request body; empty for a stream>\n
```

The body hash is taken over the deterministic marshalling of the request
message (`proto.MarshalOptions{Deterministic: true}`), so the same request
always produces the same signature input. For a streaming call the body is the
stream, which cannot be hashed before it is sent, so the hash is empty: the
signature covers the method, the timestamp and the nonce.

Implications:

- A signature of one method cannot be replayed for another, and a signature of
  one unary request cannot be replayed for another request with a different
  body.
- The timestamp bounds a captured signature to `±60s` of the server's clock.
- The nonce is remembered (`maxNonces = 100000`, oldest dropped first) and a
  repeated nonce is refused even when its signature is broken, so a captured
  request cannot be replayed inside the window.

## 4. Authentication order, and no downgrade

`authenticator.authenticate` runs one decision:

1. **A signature is present** → the caller is that agent. An unknown
   fingerprint, a bad signature, a timestamp outside the window or a repeated
   nonce is `Unauthenticated`; the request is **never** downgraded to the
   shared token.
2. **The admin secret matches** → `admin`.
3. **The shared token matches** → `shared`.
4. Otherwise, if the server requires authentication → `Unauthenticated`
   ("authentication required"). Only an upstream Gitaly without a token serves
   an anonymous caller.

The health service stays unauthenticated. `auth.transitioning` cannot be
combined with agents or with a server that requires authentication: in
transitioning mode an unauthenticated request is let through without an
identity, which would let a broken signature skip the agent allowlist.

## 5. Identity, and what the caller cannot claim

The authenticated identity is stored in the request context. From it:

- **Request defaults** (`internal/graft/requestdefaults`): for an agent, every
  `gl_id`/`gl_username` field is overwritten with the agent's name, even when
  the client sent another value. A shared-token or admin caller keeps the
  value it sent, and an empty one is filled.
- **The changeset log** records the authenticated identity as the actor: the
  agent's name, `shared` or `admin`. The request cannot choose it.
- **The policy** (`[[graft.policy]]`) decides on the agent name for an agent's
  requests, and on the request's `gl_id` for the shared token.
- **Reviews and resolutions** are attributed to the authenticated identity. A
  `Graft-Resolved-By` trailer names an agent only for an agent's signature;
  an operator's resolution keeps the unattributed `agent` value.
- Reply filters use `identity.SourceAgent` (`refs/graft/` hiding, the
  allowlist), so a shared-token caller is not mistaken for an agent.

## 6. Clients

- `sdk.WithAgentKey(path)` signs every unary and streaming call
  (`sdk.AgentSigner`).
- `graft -agent-key PATH` and `GRAFT_AGENT_KEY` do the same for the CLI.
  `GRAFT_TOKEN`/`-token` is the shared token; when both are set, the agent key
  takes precedence, because the signature is the agent's identity.
- `graft-cache` has no agent key of its own. A signed request reaches the tier
  with its signature, which the tier cannot reuse for its own upstream calls
  (the signature covers the caller's method and body). The tier forwards the
  caller's metadata unchanged; for its own authorization and fill calls it
  uses its `--token`. Without `--token`, a signed agent's reads are forwarded
  to the upstream uncached, and nothing is sent with a credential that is not
  the tier's own.
- The internal `gitaly-ssh` accepts `GRAFT_AGENT_KEY` too, and refuses to use
  it together with `GITALY_TOKEN`.

## 7. Rotation and revocation

- To rotate a key, replace the agent's `public_key`/`public_key_file` and
  restart (or reload) the server. The old key is refused as an unknown
  fingerprint.
- To revoke an agent, remove its `[[graft.agent]]` entry. Its requests are
  refused; the changeset log keeps the name it wrote under.
- The private key never leaves the agent. Nothing in the server can sign for
  it; the shared token is not a fallback for a lost key.

## 8. Limits

- The timestamp window is fixed at 60 seconds. A host whose clock drifts more
  than that is refused; run NTP.
- The nonce cache holds 100000 entries. A flood can evict an old nonce, but
  the timestamp window still bounds the replay of a captured request.
- Signing covers the method and the body, not the transport. Run the server on
  a unix socket or TLS, as for any gRPC service.
- A shared token is one identity (`shared`) for every operator. Per-agent
  identity is the point of the keys; the shared token remains a shared
  credential for tools and hooks.

## 9. Tests

- `internal/graft/agentkey/agentkey_test.go`: sign/verify, tampering, wrong
  key, fingerprint, payload stability, nonce, metadata parsing, body hash.
- `internal/gitaly/server/auth/agent_test.go` and the agent tests: the order,
  the no-downgrade rule, the window, the nonce, the allowlist, transitioning.
- `internal/graft/e2e`: a signed agent is accepted; an anonymous request, an
  unknown key and a broken signature are refused.
- `cmd/graft`, `sdk` and `internal/graft/e2e`: signing through the SDK and the
  CLI; the cache tier with and without its own token.
