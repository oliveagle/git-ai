package auth

import (
	"context"
	"testing"
	"time"

	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// TestInterceptors_AgentCannotUpdateRemoteMirror checks that an agent may not
// push a repository to a remote URL that it chooses, neither by default nor
// through [graft] agent_rpcs, which is refused at startup.
func TestInterceptors_AgentCannotUpdateRemoteMirror(t *testing.T) {
	t.Parallel()

	method := gitalypb.RemoteService_UpdateRemoteMirror_FullMethodName
	conf := gitalycfgauth.Config{Token: sharedSecretTest}

	_, err := newAgentAllowlist([]string{method})
	require.ErrorContains(t, err, "may never be called by an agent")

	agents, a, _ := testAgents(t)
	unary, _, err := Interceptors(conf, Config{Agents: agents}, true)
	require.NoError(t, err)

	_, err = unary(signedAgentContext(t, a, method), nil,
		&grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) {
			t.Fatal("the handler must not run for an agent")
			return nil, nil
		})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)

	_, _, err = Interceptors(conf, Config{Agents: agents, AgentRPCs: []string{method}}, true)
	require.ErrorContains(t, err, "may never be called by an agent")
}

// TestInterceptors_AgentCannotQueryRemoteURLs checks that the RemoteService
// RPCs that run `git ls-remote` against a URL the caller chooses are never
// allowed for agents: they are not on the built-in allowlist, are denied at
// call time, and a [graft] agent_rpcs entry naming them is refused at startup.
func TestInterceptors_AgentCannotQueryRemoteURLs(t *testing.T) {
	t.Parallel()

	conf := gitalycfgauth.Config{Token: sharedSecretTest}
	agents, a, _ := testAgents(t)
	unary, _, err := Interceptors(conf, Config{Agents: agents}, true)
	require.NoError(t, err)

	for _, method := range []string{
		gitalypb.RemoteService_FindRemoteRepository_FullMethodName,
		gitalypb.RemoteService_FindRemoteRootRef_FullMethodName,
	} {
		require.True(t, agentNeverAllowed(method), "%s", method)

		_, err := newAgentAllowlist([]string{method})
		require.ErrorContains(t, err, "may never be called by an agent", "%s", method)

		_, _, err = Interceptors(conf, Config{Agents: agents, AgentRPCs: []string{method}}, true)
		require.ErrorContains(t, err, "may never be called by an agent", "%s", method)

		_, err = unary(signedAgentContext(t, a, method), nil,
			&grpc.UnaryServerInfo{FullMethod: method},
			func(context.Context, any) (any, error) {
				t.Fatalf("the handler of %s must not run for an agent", method)
				return nil, nil
			})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%s: %v", method, err)
	}
}

// gitalyServersMetadata returns the metadata that names address for the
// storage "evil".
func gitalyServersMetadata(t *testing.T, address string) metadata.MD {
	t.Helper()

	outgoing, err := storage.InjectGitalyServers(context.Background(), "evil", address, "attacker-token")
	require.NoError(t, err)
	servers, _ := metadata.FromOutgoingContext(outgoing)

	return servers
}

// contextWithMetadataAndAuthorization returns a context with md, and with the
// authorization header when it is not empty.
func contextWithMetadataAndAuthorization(t *testing.T, md metadata.MD, authorization string) context.Context {
	t.Helper()

	if authorization != "" {
		md.Set("authorization", authorization)
	}

	return contextWithMetadata(t, md)
}

// TestInterceptors_AgentCannotSendGitalyServers checks that an agent may not
// send gitaly-servers metadata, on a unary or a streaming RPC of the built-in
// allowlist. The metadata names the address of other Gitaly servers, which
// UserCommitFiles with a start_repository on another storage dials, so it
// would let an agent make the server connect to any address. The shared token
// may still send it, as Gitaly-to-Gitaly calls do.
func TestInterceptors_AgentCannotSendGitalyServers(t *testing.T) {
	t.Parallel()

	agents, a, b := testAgents(t)
	unary, stream, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, Config{Agents: agents}, true)
	require.NoError(t, err)

	const address = "tcp://127.0.0.1:1"
	unaryMethod := gitalypb.RepositoryService_RepositoryExists_FullMethodName
	streamInfo := &grpc.StreamServerInfo{FullMethod: gitalypb.OperationService_UserCommitFiles_FullMethodName}

	runUnary := func(ctx context.Context) (bool, error) {
		called := false
		_, err := unary(ctx, &gitalypb.RepositoryExistsRequest{},
			&grpc.UnaryServerInfo{FullMethod: unaryMethod},
			func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})
		return called, err
	}

	runStream := func(ctx context.Context) (bool, error) {
		called := false
		ss := &scriptedServerStream{ctx: ctx, msgs: []proto.Message{&gitalypb.UserCommitFilesRequest{}}}
		err := stream(nil, ss, streamInfo, func(any, grpc.ServerStream) error {
			called = true
			return nil
		})
		return called, err
	}

	// agentUnaryContext returns the metadata of a signed unary call, with the
	// gitaly-servers metadata of the attacker.
	agentUnaryContext := func(key *agentKey) context.Context {
		md := gitalyServersMetadata(t, address)
		for k, v := range key.sign(t, unaryMethod, agentkey.BodyHash(&gitalypb.RepositoryExistsRequest{})) {
			md.Set(k, v...)
		}
		return contextWithMetadata(t, md)
	}

	// agentStreamContext returns the metadata of a signed stream call, with the
	// gitaly-servers metadata of the attacker.
	agentStreamContext := func(key *agentKey) context.Context {
		md := gitalyServersMetadata(t, address)
		for k, v := range key.sign(t, streamInfo.FullMethod, "") {
			md.Set(k, v...)
		}
		return contextWithMetadata(t, md)
	}

	t.Run("agent may not send gitaly-servers on a unary RPC", func(t *testing.T) {
		t.Parallel()

		called, err := runUnary(agentUnaryContext(a))
		require.Equal(t, codes.PermissionDenied, status.Code(err), "error: %v", err)
		require.ErrorContains(t, err, storage.GitalyServersMetadataKey)
		require.False(t, called)
	})

	t.Run("agent may not send gitaly-servers on UserCommitFiles", func(t *testing.T) {
		t.Parallel()

		called, err := runStream(agentStreamContext(b))
		require.Equal(t, codes.PermissionDenied, status.Code(err), "error: %v", err)
		require.False(t, called, "the handler must not run, so nothing is dialed")
	})

	t.Run("agent may call UserCommitFiles without gitaly-servers", func(t *testing.T) {
		t.Parallel()

		called, err := runStream(signedAgentContext(t, a, streamInfo.FullMethod))
		require.NoError(t, err)
		require.True(t, called)
	})

	t.Run("shared token may send gitaly-servers", func(t *testing.T) {
		t.Parallel()

		called, err := runUnary(contextWithMetadataAndAuthorization(t,
			gitalyServersMetadata(t, address), v2Authorization(sharedSecretTest, time.Now())))
		require.NoError(t, err)
		require.True(t, called)

		called, err = runStream(contextWithMetadataAndAuthorization(t,
			gitalyServersMetadata(t, address), v2Authorization(sharedSecretTest, time.Now())))
		require.NoError(t, err)
		require.True(t, called)
	})
}
