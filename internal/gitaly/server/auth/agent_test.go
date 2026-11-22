package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	agentAName       = "agent-a"
	agentBName       = "agent-b"
	sharedSecretTest = "shared-secret"
	adminSecretTest  = "admin-secret"
	testMethod       = "/gitaly.RepositoryService/RepositoryExists"
)

// testAgents returns the configuration of the two test agents, and their key
// pairs.
func testAgents(t testing.TB) ([]config.GraftAgent, *agentKey, *agentKey) {
	t.Helper()

	a := newAgentKey(t, agentAName)
	b := newAgentKey(t, agentBName)

	return []config.GraftAgent{a.agent(), b.agent()}, a, b
}

// v2Authorization returns a v2 HMAC authorization header value signed with
// secret for the given issue time.
func v2Authorization(secret string, issuedAt time.Time) string {
	message := strconv.FormatInt(issuedAt.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(message))

	return "Bearer v2." + hex.EncodeToString(mac.Sum(nil)) + "." + message
}

// callUnary runs the unary interceptor with a handler that reports the identity
// found in the context of the handler.
func callUnary(t *testing.T, ctx context.Context, interceptor grpc.UnaryServerInterceptor, srv any) (identity.Identity, bool, error) {
	t.Helper()

	var got identity.Identity
	var found bool
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: testMethod, Server: srv},
		func(ctx context.Context, _ any) (any, error) {
			got, found = identity.FromContext(ctx)
			return nil, nil
		})

	return got, found, err
}

// contextWithAuthorization returns a context with an authorization header.
func contextWithAuthorization(t *testing.T, authorization string) context.Context {
	t.Helper()

	ctx := testhelper.Context(t)
	if authorization == "" {
		return ctx
	}

	return metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", authorization))
}

func TestInterceptors_AgentSignatures(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sharedConf := gitalycfgauth.Config{Token: sharedSecretTest}

	testCases := []struct {
		desc          string
		conf          gitalycfgauth.Config
		graft         Config
		external      bool
		server        any
		authorization string
		// signature is the metadata of the signed request. It is built per
		// test case, because the key pairs are random.
		signature func(t *testing.T, a, b *agentKey) metadata.MD
		wantCode  codes.Code
		wantID    identity.Identity
		wantFound bool
	}{
		{
			desc:     "the signature of an agent is accepted",
			conf:     sharedConf,
			external: true,
			signature: func(_ *testing.T, a, _ *agentKey) metadata.MD {
				return a.sign(t, testMethod, "")
			},
			wantCode:  codes.OK,
			wantID:    identity.Identity{Name: agentAName, Source: identity.SourceAgent},
			wantFound: true,
		},
		{
			desc:     "the signature of the second agent is accepted",
			conf:     sharedConf,
			external: true,
			signature: func(_ *testing.T, _, b *agentKey) metadata.MD {
				return b.sign(t, testMethod, "")
			},
			wantCode:  codes.OK,
			wantID:    identity.Identity{Name: agentBName, Source: identity.SourceAgent},
			wantFound: true,
		},
		{
			desc:     "a signature of another method is refused",
			conf:     sharedConf,
			external: true,
			signature: func(_ *testing.T, a, _ *agentKey) metadata.MD {
				return a.sign(t, "/gitaly.RepositoryService/RepositorySize", "")
			},
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "a signature of another request is refused",
			conf:     sharedConf,
			external: true,
			signature: func(_ *testing.T, a, _ *agentKey) metadata.MD {
				return a.sign(t, testMethod, "hash of another request")
			},
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "an unknown key is refused",
			conf:     sharedConf,
			external: true,
			signature: func(t *testing.T, _, _ *agentKey) metadata.MD {
				other := newAgentKey(t, "stranger")
				return other.sign(t, testMethod, "")
			},
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "a signature of a key the server does not know is refused",
			conf:     sharedConf,
			external: true,
			signature: func(t *testing.T, a, _ *agentKey) metadata.MD {
				md := a.sign(t, testMethod, "")
				md.Set(agentkey.KeyMetadata, "SHA256:unknown")
				return md
			},
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "a signature of the past is refused",
			conf:     sharedConf,
			external: true,
			signature: func(t *testing.T, a, _ *agentKey) metadata.MD {
				return a.signAt(t, testMethod, "", now.Add(-time.Hour))
			},
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "a signature of the future is refused",
			conf:     sharedConf,
			external: true,
			signature: func(t *testing.T, a, _ *agentKey) metadata.MD {
				return a.signAt(t, testMethod, "", now.Add(time.Hour))
			},
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "an incomplete signature is refused",
			conf:     sharedConf,
			external: true,
			signature: func(t *testing.T, a, _ *agentKey) metadata.MD {
				md := a.sign(t, testMethod, "")
				md.Set(agentkey.SignatureMetadata, "")
				return md
			},
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "a broken signature is refused",
			conf:     sharedConf,
			external: true,
			signature: func(t *testing.T, a, _ *agentKey) metadata.MD {
				md := a.sign(t, testMethod, "")
				md.Set(agentkey.SignatureMetadata, "bm90LWEtc2lnbmF0dXJl")
				return md
			},
			wantCode: codes.Unauthenticated,
		},
		{
			desc:          "the shared token is accepted next to the agents",
			conf:          sharedConf,
			external:      true,
			authorization: v2Authorization(sharedSecretTest, now),
			wantCode:      codes.OK,
			wantID:        identity.Identity{Name: sharedTokenIdentityName, Source: identity.SourceSharedToken},
			wantFound:     true,
		},
		{
			desc:          "the admin token is accepted as the super admin",
			conf:          sharedConf,
			graft:         Config{AdminToken: adminSecretTest},
			external:      true,
			authorization: v2Authorization(adminSecretTest, now),
			wantCode:      codes.OK,
			wantID:        identity.Identity{Name: adminIdentityName, Source: identity.SourceAdmin},
			wantFound:     true,
		},
		{
			desc:     "a request with no credential is refused",
			conf:     sharedConf,
			external: true,
			wantCode: codes.Unauthenticated,
		},
		{
			desc:     "the health check is not authenticated",
			conf:     sharedConf,
			external: true,
			server:   UnauthenticatedHealthService{},
			wantCode: codes.OK,
		},
		{
			desc:          "the internal server accepts the shared token",
			conf:          sharedConf,
			external:      false,
			authorization: v2Authorization(sharedSecretTest, now),
			wantCode:      codes.OK,
			wantID:        identity.Identity{Name: sharedTokenIdentityName, Source: identity.SourceSharedToken},
			wantFound:     true,
		},
		{
			desc:     "the internal server refuses a request without a credential",
			conf:     sharedConf,
			external: false,
			wantCode: codes.Unauthenticated,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			agents, a, b := testAgents(t)
			graft := tc.graft
			graft.Agents = agents

			unary, _, err := Interceptors(tc.conf, graft, tc.external)
			require.NoError(t, err)

			var md metadata.MD
			if tc.signature != nil {
				md = tc.signature(t, a, b)
			}

			ctx := contextWithSignature(t, md, tc.authorization)
			id, found, err := callUnary(t, ctx, unary, tc.server)
			require.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
			require.Equal(t, tc.wantFound, found)
			if tc.wantFound {
				require.Equal(t, tc.wantID, id)
			}
		})
	}
}

// TestInterceptors_ReplayedSignatureIsRefused checks that a captured request
// cannot be sent twice.
func TestInterceptors_ReplayedSignatureIsRefused(t *testing.T) {
	t.Parallel()

	agents, a, _ := testAgents(t)
	unary, _, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, Config{Agents: agents}, true)
	require.NoError(t, err)

	md := a.sign(t, testMethod, "")
	ctx := contextWithSignature(t, md, "")

	_, found, err := callUnary(t, ctx, unary, nil)
	require.NoError(t, err)
	require.True(t, found)

	// The same signature a second time is a replay.
	_, found, err = callUnary(t, ctx, unary, nil)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	require.False(t, found)
}

// TestInterceptors_SignatureIsNotDowngraded checks that a request with a
// broken signature is refused even when it carries the shared token: a signed
// request is authenticated with its signature and with nothing else.
func TestInterceptors_SignatureIsNotDowngraded(t *testing.T) {
	t.Parallel()

	agents, a, _ := testAgents(t)
	unary, _, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, Config{Agents: agents}, true)
	require.NoError(t, err)

	md := a.sign(t, testMethod, "")
	md.Set(agentkey.SignatureMetadata, "bm90LWEtc2lnbmF0dXJl")

	ctx := contextWithSignature(t, md, v2Authorization(sharedSecretTest, time.Now()))
	_, found, err := callUnary(t, ctx, unary, nil)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	require.False(t, found)
}

// TestInterceptors_AnonymousIsRefused checks that a server that requires
// authentication refuses the callers that present no credential, and that an
// upstream Gitaly without a token keeps its behaviour.
func TestInterceptors_AnonymousIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("a Graft server refuses anonymous callers", func(t *testing.T) {
		t.Parallel()

		unary, _, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, Config{RequireAuthentication: true}, true)
		require.NoError(t, err)

		_, found, err := callUnary(t, testhelper.Context(t), unary, nil)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.False(t, found)
	})

	t.Run("an upstream Gitaly without a token serves anonymous callers", func(t *testing.T) {
		t.Parallel()

		unary, _, err := Interceptors(gitalycfgauth.Config{}, Config{}, true)
		require.NoError(t, err)

		_, found, err := callUnary(t, testhelper.Context(t), unary, nil)
		require.NoError(t, err)
		require.False(t, found)
	})

	t.Run("a server that requires authentication and has no credential is a configuration error", func(t *testing.T) {
		t.Parallel()

		_, _, err := Interceptors(gitalycfgauth.Config{}, Config{RequireAuthentication: true}, true)
		require.ErrorContains(t, err, "no credential is configured")
	})
}

func TestInterceptors_StreamAgentSignature(t *testing.T) {
	t.Parallel()

	agents, _, b := testAgents(t)
	_, stream, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, Config{Agents: agents}, true)
	require.NoError(t, err)

	t.Run("the identity of the agent reaches the handler", func(t *testing.T) {
		t.Parallel()

		ctx := contextWithSignature(t, b.sign(t, testMethod, ""), "")

		var got identity.Identity
		var found bool
		err := stream(nil, fakeServerStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: testMethod},
			func(_ any, ss grpc.ServerStream) error {
				got, found = identity.FromContext(ss.Context())
				return nil
			})

		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, identity.Identity{Name: agentBName, Source: identity.SourceAgent}, got)
	})

	t.Run("a request without a signature is rejected before the handler", func(t *testing.T) {
		t.Parallel()

		ctx := contextWithAuthorization(t, "Bearer wrong")

		called := false
		err := stream(nil, fakeServerStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: testMethod},
			func(_ any, _ grpc.ServerStream) error {
				called = true
				return nil
			})

		require.Equal(t, codes.Unauthenticated, status.Code(err))
		require.False(t, called)
	})
}

type fakeServerStream struct {
	grpc.ServerStream

	ctx context.Context
}

func (s fakeServerStream) Context() context.Context { return s.ctx }

func TestLoadAgentCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a := newAgentKey(t, "a")
	b := newAgentKey(t, "b")

	keyFile := filepath.Join(dir, "agent.pub")
	require.NoError(t, os.WriteFile(keyFile, []byte(a.publicKey+"\n"), 0o600))

	brokenFile := filepath.Join(dir, "broken.pub")
	require.NoError(t, os.WriteFile(brokenFile, []byte("not a key\n"), 0o600))

	emptyFile := filepath.Join(dir, "empty.pub")
	require.NoError(t, os.WriteFile(emptyFile, []byte(" \n"), 0o600))

	testCases := []struct {
		desc    string
		agents  []config.GraftAgent
		want    []agentCredential
		wantErr string
	}{
		{
			desc:   "no agents",
			agents: nil,
			want:   []agentCredential{},
		},
		{
			desc:   "an inline public key",
			agents: []config.GraftAgent{a.agent()},
			want:   []agentCredential{{name: a.name, fingerprint: a.fingerprint}},
		},
		{
			desc:   "the public key file is read",
			agents: []config.GraftAgent{{Name: "a", PublicKeyFile: keyFile}},
			want:   []agentCredential{{name: "a", fingerprint: a.fingerprint}},
		},
		{
			desc:    "an inline key that is not a key",
			agents:  []config.GraftAgent{{Name: "a", PublicKey: "not a key"}},
			wantErr: "parse the public key",
		},
		{
			desc:    "a key file that holds no key",
			agents:  []config.GraftAgent{{Name: "a", PublicKeyFile: brokenFile}},
			wantErr: "parse the public key file",
		},
		{
			desc:    "an empty key file",
			agents:  []config.GraftAgent{{Name: "a", PublicKeyFile: emptyFile}},
			wantErr: "parse the public key file",
		},
		{
			desc:    "an empty name",
			agents:  []config.GraftAgent{{Name: "", PublicKey: a.publicKey}},
			wantErr: "name must not be empty",
		},
		{
			desc:    "the reserved name of the shared token",
			agents:  []config.GraftAgent{{Name: sharedTokenIdentityName, PublicKey: a.publicKey}},
			wantErr: "is reserved",
		},
		{
			desc:    "the reserved name of the super admin",
			agents:  []config.GraftAgent{{Name: adminIdentityName, PublicKey: a.publicKey}},
			wantErr: "is reserved",
		},
		{
			desc:    "the reserved name of the proposals",
			agents:  []config.GraftAgent{{Name: "proposals", PublicKey: a.publicKey}},
			wantErr: "is reserved for the server's references under refs/graft/",
		},
		{
			desc:    "the reserved name of the green references",
			agents:  []config.GraftAgent{{Name: "green", PublicKey: a.publicKey}},
			wantErr: "is reserved for the server's references under refs/graft/",
		},
		{
			desc:    "a duplicate name",
			agents:  []config.GraftAgent{a.agent(), {Name: a.name, PublicKey: b.publicKey}},
			wantErr: "duplicate name",
		},
		{
			desc:    "a duplicate key",
			agents:  []config.GraftAgent{a.agent(), {Name: "b", PublicKey: a.publicKey}},
			wantErr: "already used by another agent",
		},
		{
			desc:    "the key and the key file are mutually exclusive",
			agents:  []config.GraftAgent{{Name: "a", PublicKey: a.publicKey, PublicKeyFile: keyFile}},
			wantErr: "mutually exclusive",
		},
		{
			desc:    "neither a key nor a key file",
			agents:  []config.GraftAgent{{Name: "a"}},
			wantErr: "either public_key or public_key_file must be set",
		},
		{
			desc:    "a missing key file",
			agents:  []config.GraftAgent{{Name: "a", PublicKeyFile: filepath.Join(dir, "missing")}},
			wantErr: "read the public key file",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			got, err := loadAgentCredentials(tc.agents)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.Len(t, got, len(tc.want))
			for i, cred := range got {
				require.Equal(t, tc.want[i].name, cred.name)
				require.Equal(t, tc.want[i].fingerprint, cred.fingerprint)
				require.NotNil(t, cred.key)
			}
		})
	}
}

func TestResolveAdminToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "admin-token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("  file-secret \n"), 0o600))

	t.Run("an inline token", func(t *testing.T) {
		t.Parallel()

		secret, err := resolveAdminToken("inline", "")
		require.NoError(t, err)
		require.Equal(t, "inline", secret)
	})

	t.Run("the token file is read and trimmed", func(t *testing.T) {
		t.Parallel()

		secret, err := resolveAdminToken("", tokenFile)
		require.NoError(t, err)
		require.Equal(t, "file-secret", secret)
	})

	t.Run("the token and the token file are mutually exclusive", func(t *testing.T) {
		t.Parallel()

		_, err := resolveAdminToken("inline", tokenFile)
		require.ErrorContains(t, err, "mutually exclusive")
	})

	t.Run("a missing token file", func(t *testing.T) {
		t.Parallel()

		_, err := resolveAdminToken("", filepath.Join(dir, "missing"))
		require.ErrorContains(t, err, "read the admin token file")
	})
}

func TestInterceptors_InvalidAgentConfigFailsAtStartup(t *testing.T) {
	t.Parallel()

	_, _, err := Interceptors(gitalycfgauth.Config{}, Config{Agents: []config.GraftAgent{{Name: "a"}}}, true)
	require.ErrorContains(t, err, "either public_key or public_key_file must be set")

	// The internal server uses the same configuration, so a broken agent
	// configuration is reported there too: a Gitaly instance without external
	// listeners does not start with it unnoticed.
	_, _, err = Interceptors(gitalycfgauth.Config{}, Config{Agents: []config.GraftAgent{{Name: "a"}}}, false)
	require.ErrorContains(t, err, "either public_key or public_key_file must be set")

	// An unreadable key file is reported by the internal server as well.
	missing := filepath.Join(t.TempDir(), "missing")
	_, _, err = Interceptors(gitalycfgauth.Config{}, Config{Agents: []config.GraftAgent{{Name: "a", PublicKeyFile: missing}}}, false)
	require.ErrorContains(t, err, "read the public key file")

	// Without agents the server is unchanged.
	_, _, err = Interceptors(gitalycfgauth.Config{}, Config{}, false)
	require.NoError(t, err)

	// An unreadable admin token file is reported at startup, on both servers.
	for _, external := range []bool{true, false} {
		_, _, err = Interceptors(gitalycfgauth.Config{}, Config{AdminTokenFile: missing}, external)
		require.ErrorContains(t, err, "read the admin token file")
	}
}

func TestInterceptors_ConcurrentAuthentication(t *testing.T) {
	t.Parallel()

	agents, a, b := testAgents(t)
	unary, _, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, Config{Agents: agents}, true)
	require.NoError(t, err)

	const workers = 50
	const iterations = 20

	var wg sync.WaitGroup
	errs := make(chan error, workers*iterations)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			key := a
			name := agentAName
			if i%2 == 1 {
				key = b
				name = agentBName
			}

			for j := 0; j < iterations; j++ {
				ctx := metadata.NewIncomingContext(context.Background(), key.sign(t, testMethod, ""))

				var got identity.Identity
				_, err := unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: testMethod},
					func(ctx context.Context, _ any) (any, error) {
						got, _ = identity.FromContext(ctx)
						return nil, nil
					})
				if err != nil {
					errs <- err
					continue
				}
				if got.Name != name {
					errs <- fmt.Errorf("worker %d: identity %q, want %q", i, got.Name, name)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	var all []error
	for err := range errs {
		all = append(all, err)
	}
	require.NoError(t, errors.Join(all...))
	assert.Empty(t, all)
}
