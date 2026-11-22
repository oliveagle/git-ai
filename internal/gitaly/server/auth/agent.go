package auth

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/feder-cr/git-ai/internal/graft/agentrefs"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	grpcmwauth "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// sharedTokenIdentityName is the identity name of callers authenticated with
// the shared auth.token. Agents may not use this name.
const sharedTokenIdentityName = "shared"

// adminIdentityName is the identity name of the super admin, the caller
// authenticated with the graft.admin_token. Only the super admin may purge an
// archived repository (doc/graft/PLAN13.md).
const adminIdentityName = "admin"

// signatureWindow is how far the time of a signed request may be from the
// clock of the server. A request outside the window is refused: a captured
// request must not be replayable much later.
const signatureWindow = 60 * time.Second

// maxNonces is the number of nonces the server remembers. The oldest entry is
// dropped when the cache grows over the limit, so that a flood of requests
// cannot make the server forget the nonces it must refuse.
const maxNonces = 100000

// Config is the Graft part of the authentication configuration of a server.
type Config struct {
	// Agents are the agent credentials the server accepts. Every agent is an
	// SSH key pair: the server holds the public key and verifies the
	// signature of every request of the agent.
	Agents []config.GraftAgent
	// AgentRPCs are the extra RPCs an agent may call, on top of the built-in
	// allowlist.
	AgentRPCs []string
	// AdminToken is the secret of the super admin.
	AdminToken string
	// AdminTokenFile is a file holding the secret of the super admin.
	AdminTokenFile string
	// RequireAuthentication refuses every request that carries no
	// credential. A Graft server sets it: anonymous access is not allowed.
	RequireAuthentication bool
}

// agentCredential is a resolved [[graft.agent]] credential: the public key of
// the agent and the identity its requests carry.
type agentCredential struct {
	// name is the identity of the agent.
	name string
	// key is the public key the server verifies the signatures with.
	key ssh.PublicKey
	// fingerprint is the fingerprint of the key, the name the server looks
	// the agent up with.
	fingerprint string
}

// Interceptors returns the authentication interceptors of a gRPC server.
//
// Every request must carry a credential, unless the server is an upstream
// Gitaly without a token: a Graft server (see Config.RequireAuthentication)
// never serves an anonymous caller. A request is authenticated with one of:
//
//   - the signature of an agent ([[graft.agent]]), which the server verifies
//     with the public key of the agent. The identity of the caller is the
//     agent the key belongs to, never a name the request carries;
//   - the super admin's secret (graft.admin_token), which identifies the
//     caller as the super admin;
//   - the shared token (auth.token), which identifies the caller as the
//     operator.
//
// A request that presents an agent signature is authenticated with it and
// with nothing else: a broken signature is never downgraded to a weaker
// credential. The accepted caller is stored in the request context as an
// identity.Identity.
//
// A caller authenticated as an agent may only call the RPCs of the allowlist
// (see builtinAgentRPCs and agentAllowlist), plus the [graft] agent_rpcs of
// the configuration. Every other RPC is rejected with PermissionDenied, so
// administrative RPCs stay reserved for the operator. Agents may also not
// send requests carrying git_config_options (see checkAgentRequest) or
// gitaly-servers metadata (see checkAgentMetadata). All of these are rejected
// with PermissionDenied.
//
// The agent configuration is validated and the key files are read on every
// call, including for the internal server, so an unreadable or invalid agent
// configuration is reported at startup even when no external server is
// created. Health checks stay unauthenticated.
func Interceptors(conf gitalycfgauth.Config, graft Config, external bool) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor, error) {
	allowlist, err := newAgentAllowlist(graft.AgentRPCs)
	if err != nil {
		return nil, nil, err
	}

	creds, err := loadAgentCredentials(graft.Agents)
	if err != nil {
		return nil, nil, err
	}

	adminToken, err := resolveAdminToken(graft.AdminToken, graft.AdminTokenFile)
	if err != nil {
		return nil, nil, err
	}

	// In transitioning mode a request that fails authentication carries no
	// identity and is let through. With agents configured that would also let
	// a request with a wrong signature skip the allowlist, so the two cannot
	// be combined. A server that refuses anonymous requests has no use for
	// the mode either.
	if conf.Transitioning && len(creds) > 0 {
		return nil, nil, fmt.Errorf("auth.transitioning must be false when [[graft.agent]] is configured: a request with an invalid signature would skip the allowlist")
	}
	if conf.Transitioning && graft.RequireAuthentication {
		return nil, nil, fmt.Errorf("auth.transitioning must be false when the server requires authentication: a request without a credential would be let through")
	}

	auth := &authenticator{
		sharedToken:   conf.GetToken(),
		adminToken:    adminToken,
		agents:        creds,
		byFingerprint: fingerprintIndex(creds),
		requireAuth:   graft.RequireAuthentication,
		transitioning: conf.Transitioning,
		nonces:        newNonceCache(),
	}

	// A server that refuses anonymous requests and has no credential at all
	// would refuse every request. That is a configuration error, not a
	// silently anonymous server.
	if graft.RequireAuthentication && auth.sharedToken == "" && adminToken == "" && len(creds) == 0 {
		return nil, nil, fmt.Errorf("no credential is configured: a Graft server refuses anonymous requests, so it needs auth.token, a [[graft.agent]] or graft.admin_token")
	}

	authUnary, authStream := withAgentAuthorization(allowlist, auth.unaryInterceptor(), auth.streamInterceptor())

	if !external {
		return authUnary, authStream, nil
	}

	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return authUnary(ctx, req, info, func(ctx context.Context, req any) (any, error) {
			if err := checkAgentMetadata(ctx); err != nil {
				return nil, err
			}
			if err := checkAgentRequest(ctx, req); err != nil {
				return nil, err
			}
			if err := checkAgentRefs(ctx, req); err != nil {
				return nil, err
			}
			resp, err := handler(ctx, req)
			if err == nil {
				filterAgentRefs(ctx, resp)
			}
			return resp, err
		})
	}

	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return authStream(srv, ss, info, func(srv any, ss grpc.ServerStream) error {
			if err := checkAgentMetadata(ss.Context()); err != nil {
				return err
			}
			return handler(srv, &agentRequestGuardStream{ServerStream: ss})
		})
	}

	return unary, stream, nil
}

// authenticator authenticates the callers of a server.
type authenticator struct {
	// sharedToken is the auth.token: the credential of the operator.
	sharedToken string
	// adminToken is the graft.admin_token: the credential of the super
	// admin.
	adminToken string
	// agents are the agent credentials, in configuration order.
	agents []agentCredential
	// byFingerprint indexes the agents by the fingerprint of their key.
	byFingerprint map[string]agentCredential
	// requireAuth refuses a request that carries no credential.
	requireAuth bool
	// transitioning lets a request that fails authentication through without
	// an identity.
	transitioning bool
	// nonces remembers the nonces of the signed requests, so that a captured
	// request cannot be replayed.
	nonces *nonceCache
}

// unaryInterceptor returns the interceptor that authenticates a unary
// request and stores the identity of the caller in the context of the
// handler.
func (a *authenticator) unaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		newCtx, err := a.authenticateContext(ctx, info.Server, info.FullMethod, agentkey.BodyHash(req))
		if err != nil {
			return nil, err
		}

		return handler(newCtx, req)
	}
}

// streamInterceptor returns the interceptor that authenticates a streaming
// request and stores the identity of the caller in the context of the
// stream.
func (a *authenticator) streamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		newCtx, err := a.authenticateContext(ss.Context(), srv, info.FullMethod, "")
		if err != nil {
			return err
		}

		return handler(srv, &serverStreamWithContext{ServerStream: ss, ctx: newCtx})
	}
}

// authenticateContext authenticates the caller of the request and returns the
// context the handler must run with. The identity of the caller is stored in
// it.
func (a *authenticator) authenticateContext(ctx context.Context, srv any, method, bodyHash string) (context.Context, error) {
	// A service may override the authentication of its methods, as the health
	// service does: its checks are not authenticated.
	if override, ok := srv.(grpcmwauth.ServiceAuthFuncOverride); ok {
		return override.AuthFuncOverride(ctx, method)
	}

	id, err := a.authenticate(ctx, method, bodyHash, time.Now())
	switch status.Code(err) {
	case codes.OK:
		countStatus(okLabel(a.transitioning), a.transitioning).Inc()
	default:
		countStatus("unauthenticated", a.transitioning).Inc()
	}

	if err != nil {
		if a.transitioning {
			return ctx, nil
		}
		return ctx, err
	}

	if id.Name == "" {
		return ctx, nil
	}

	return identity.NewContext(ctx, id), nil
}

// authenticate returns the identity of the caller of the request.
//
// The method and the hash of the request are part of the signed payload, so
// that a signature of another request cannot be replayed for this one.
func (a *authenticator) authenticate(ctx context.Context, method, bodyHash string, now time.Time) (identity.Identity, error) {
	signature, err := agentkey.SignatureFromMetadata(ctx)
	if err != nil {
		return identity.Identity{}, status.Error(codes.Unauthenticated, err.Error())
	}

	// A signed request is authenticated with its signature and with nothing
	// else: a request that presents a signature is never downgraded to a
	// weaker credential.
	if signature != nil {
		return a.authenticateAgent(ctx, signature, method, bodyHash, now)
	}

	if a.adminToken != "" && gitalyauth.CheckToken(ctx, a.adminToken, now) == nil {
		return identity.Identity{Name: adminIdentityName, Source: identity.SourceAdmin}, nil
	}

	if a.sharedToken != "" && gitalyauth.CheckToken(ctx, a.sharedToken, now) == nil {
		return identity.Identity{Name: sharedTokenIdentityName, Source: identity.SourceSharedToken}, nil
	}

	if !a.requireAuth && a.sharedToken == "" {
		// An upstream Gitaly without a token: every caller is anonymous.
		return identity.Identity{}, nil
	}

	return identity.Identity{}, status.Error(codes.Unauthenticated, "authentication required")
}

// authenticateAgent verifies the signature of a request and returns the
// identity of the agent that signed it.
func (a *authenticator) authenticateAgent(ctx context.Context, signature *agentkey.Signature, method, bodyHash string, now time.Time) (identity.Identity, error) {
	cred, ok := a.byFingerprint[signature.Fingerprint]
	if !ok {
		return identity.Identity{}, status.Errorf(codes.Unauthenticated, "unknown agent key %q", signature.Fingerprint)
	}

	if delta := now.Sub(time.Unix(signature.Timestamp, 0)); delta > signatureWindow || delta < -signatureWindow {
		return identity.Identity{}, status.Errorf(codes.Unauthenticated, "the signature of agent %q is outside the validity window", cred.name)
	}

	// The nonce is remembered before the signature is verified, so that a
	// captured request cannot be replayed even when its signature is broken.
	if !a.nonces.add(signature.Nonce, now.Add(signatureWindow)) {
		return identity.Identity{}, status.Errorf(codes.Unauthenticated, "the signature of agent %q was already used", cred.name)
	}

	payload := agentkey.Payload(signature.Timestamp, signature.Nonce, method, bodyHash)
	if err := agentkey.Verify(cred.key, payload, signature.Value); err != nil {
		return identity.Identity{}, status.Errorf(codes.Unauthenticated, "the signature of agent %q is invalid", cred.name)
	}

	return identity.Identity{Name: cred.name, Source: identity.SourceAgent}, nil
}

// serverStreamWithContext is a server stream whose context carries the
// identity of the caller.
type serverStreamWithContext struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the context of the stream, with the identity of the caller.
func (s *serverStreamWithContext) Context() context.Context { return s.ctx }

// fingerprintIndex indexes the agents by the fingerprint of their public key.
func fingerprintIndex(creds []agentCredential) map[string]agentCredential {
	index := make(map[string]agentCredential, len(creds))
	for _, cred := range creds {
		index[cred.fingerprint] = cred
	}
	return index
}

// nonceCache remembers the nonces of the signed requests, so that a captured
// request cannot be replayed. The entries expire with the validity window of
// the signatures.
type nonceCache struct {
	mutex  sync.Mutex
	nonces map[string]time.Time
}

// newNonceCache returns an empty cache.
func newNonceCache() *nonceCache {
	return &nonceCache{nonces: make(map[string]time.Time)}
}

// add remembers the nonce and reports whether it was new. The entry that
// expires first is dropped when the cache is full: expired nonces are
// forgotten first, and an expired nonce of an old request is no longer needed
// because its timestamp is outside the window.
func (c *nonceCache) add(nonce string, expiry time.Time) bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if _, ok := c.nonces[nonce]; ok {
		return false
	}

	if len(c.nonces) >= maxNonces {
		now := time.Now()
		for seen, expires := range c.nonces {
			if expires.Before(now) {
				delete(c.nonces, seen)
			}
		}
	}
	if len(c.nonces) >= maxNonces {
		c.dropOldest()
	}

	c.nonces[nonce] = expiry
	return true
}

// dropOldest forgets the nonce that expires first. The caller holds the lock.
func (c *nonceCache) dropOldest() {
	var oldest string
	var expires time.Time
	for nonce, expiry := range c.nonces {
		if oldest == "" || expiry.Before(expires) {
			oldest, expires = nonce, expiry
		}
	}

	if oldest != "" {
		delete(c.nonces, oldest)
	}
}

// checkAgentMetadata rejects a request of an agent that carries metadata agents
// may not send. Other identities are not restricted.
//
// Agents may not send gitaly-servers metadata. It maps storage names to the
// address and token of a Gitaly server, and RPCs that work across repositories,
// such as UserCommitFiles with a start_repository on another storage, dial the
// address it names and fetch from it. An agent could therefore make the server
// connect to any address it likes, including internal network addresses and
// Unix sockets (server-side request forgery). Without the metadata, such a
// request fails before anything is dialed.
func checkAgentMetadata(ctx context.Context) error {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Source != identity.SourceAgent {
		return nil
	}

	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get(storage.GitalyServersMetadataKey)) > 0 {
		return status.Errorf(codes.PermissionDenied, "agent %q may not send %s metadata", id.Name, storage.GitalyServersMetadataKey)
	}

	return nil
}

// gitConfigOptionsFieldName is the name of the request field whose values are
// passed to Git as `-c key=value` options (SSH and smart HTTP RPCs).
const gitConfigOptionsFieldName = protoreflect.Name("git_config_options")

// checkAgentRequest rejects a request message of an agent that carries
// options agents may not set. Other identities are not restricted.
//
// Agents may not send git_config_options: Git applies these options after
// the ones Gitaly sets, so they could for example override core.hooksPath to
// skip the access checks and the reference-transaction hook of a push, or set
// uploadpack.packObjectsHook to run an arbitrary command.
func checkAgentRequest(ctx context.Context, req any) error {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Source != identity.SourceAgent {
		return nil
	}

	msg, ok := req.(proto.Message)
	if !ok {
		return nil
	}

	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(gitConfigOptionsFieldName)
	if fd != nil && fd.IsList() && m.Get(fd).List().Len() > 0 {
		return status.Errorf(codes.PermissionDenied, "agent %q may not set %s", id.Name, gitConfigOptionsFieldName)
	}

	return nil
}

// agentRequestGuardStream checks every message an agent sends on a stream, and
// removes from every message it receives the references it may not see.
type agentRequestGuardStream struct {
	grpc.ServerStream
}

// RecvMsg receives the next message and rejects it when the agent may not
// send it.
func (s *agentRequestGuardStream) RecvMsg(m any) error {
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return err
	}

	if err := checkAgentRequest(s.Context(), m); err != nil {
		return err
	}
	return checkAgentRefs(s.Context(), m)
}

// SendMsg sends m without the references under refs/graft/ it lists.
func (s *agentRequestGuardStream) SendMsg(m any) error {
	filterAgentRefs(s.Context(), m)
	return s.ServerStream.SendMsg(m)
}

// loadAgentCredentials validates the agent configuration and resolves the
// public key of every agent, reading key files from disk.
func loadAgentCredentials(agents []config.GraftAgent) ([]agentCredential, error) {
	creds := make([]agentCredential, 0, len(agents))
	names := make(map[string]struct{}, len(agents))
	fingerprints := make(map[string]struct{}, len(agents))

	for i, agent := range agents {
		if agent.Name == "" {
			return nil, fmt.Errorf("graft agent %d: name must not be empty", i)
		}
		if agent.Name == sharedTokenIdentityName || agent.Name == adminIdentityName {
			return nil, fmt.Errorf("graft agent %d: name %q is reserved", i, agent.Name)
		}
		// The workspace's temporary branches of an agent are graft/<name>/..., and an agent may not
		// name graft/<namespace>/... for a namespace of the server (agentrefs): an agent named after
		// one could not use its own branches.
		if slices.Contains(agentrefs.ServerNamespaces, agent.Name) {
			return nil, fmt.Errorf("graft agent %d: name %q is reserved for the server's references under %s", i, agent.Name, agentrefs.HiddenPrefix)
		}
		if _, ok := names[agent.Name]; ok {
			return nil, fmt.Errorf("graft agent %q: duplicate name", agent.Name)
		}
		names[agent.Name] = struct{}{}

		key, err := resolveAgentKey(agent)
		if err != nil {
			return nil, fmt.Errorf("graft agent %q: %w", agent.Name, err)
		}

		// Two agents with the same key would be the same caller: the
		// signature would not tell them apart.
		fingerprint := agentkey.Fingerprint(key)
		if _, ok := fingerprints[fingerprint]; ok {
			return nil, fmt.Errorf("graft agent %q: the public key is already used by another agent", agent.Name)
		}
		fingerprints[fingerprint] = struct{}{}

		creds = append(creds, agentCredential{name: agent.Name, key: key, fingerprint: fingerprint})
	}

	return creds, nil
}

// resolveAgentKey returns the public key of an agent: the one in the
// configuration or the one in the file it names.
func resolveAgentKey(agent config.GraftAgent) (ssh.PublicKey, error) {
	switch {
	case agent.PublicKey != "" && agent.PublicKeyFile != "":
		return nil, fmt.Errorf("public_key and public_key_file are mutually exclusive")
	case agent.PublicKeyFile != "":
		data, err := os.ReadFile(agent.PublicKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read the public key file: %w", err)
		}
		key, err := agentkey.ParsePublicKey(data)
		if err != nil {
			return nil, fmt.Errorf("parse the public key file %q: %w", agent.PublicKeyFile, err)
		}
		return key, nil
	case agent.PublicKey != "":
		key, err := agentkey.ParsePublicKey([]byte(agent.PublicKey))
		if err != nil {
			return nil, fmt.Errorf("parse the public key: %w", err)
		}
		return key, nil
	default:
		return nil, fmt.Errorf("either public_key or public_key_file must be set")
	}
}

// resolveAdminToken returns the secret of the super admin: the value of the
// configuration or the content of the file it names.
func resolveAdminToken(token, tokenFile string) (string, error) {
	switch {
	case token != "" && tokenFile != "":
		return "", fmt.Errorf("graft: admin_token and admin_token_file are mutually exclusive")
	case tokenFile != "":
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("graft: read the admin token file: %w", err)
		}
		secret := strings.TrimSpace(string(data))
		if secret == "" {
			return "", fmt.Errorf("graft: the admin token file %q is empty", tokenFile)
		}
		return secret, nil
	default:
		return token, nil
	}
}
