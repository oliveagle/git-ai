package auth_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/bootstrap/starter"
	"github.com/feder-cr/git-ai/internal/cache"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/server"
	"github.com/feder-cr/git-ai/internal/gitaly/server/auth"
	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// identityEchoServer reports the identity the auth interceptor stored in the
// request context through ServerInfo's server_version.
type identityEchoServer struct {
	gitalypb.UnimplementedServerServiceServer
}

func (identityEchoServer) ServerInfo(ctx context.Context, _ *gitalypb.ServerInfoRequest) (*gitalypb.ServerInfoResponse, error) {
	version := "no identity"
	if id, ok := identity.FromContext(ctx); ok {
		version = fmt.Sprintf("%s/%s", id.Name, id.Source)
	}

	return &gitalypb.ServerInfoResponse{ServerVersion: version}, nil
}

// identityClaimServer reports the identity fields of the User of a
// UserCreateBranch request, which the request defaults filled in.
type identityClaimServer struct {
	gitalypb.UnimplementedOperationServiceServer
}

func (identityClaimServer) UserCreateBranch(_ context.Context, req *gitalypb.UserCreateBranchRequest) (*gitalypb.UserCreateBranchResponse, error) {
	return &gitalypb.UserCreateBranchResponse{
		Branch: &gitalypb.Branch{Name: []byte(req.GetUser().GetGlId() + "/" + req.GetUser().GetGlUsername())},
	}, nil
}

// UserCommitFiles reports the identity fields of the User nested in the header
// of the first message of the stream.
func (identityClaimServer) UserCommitFiles(stream gitalypb.OperationService_UserCommitFilesServer) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}

	user := req.GetHeader().GetUser()
	return stream.SendAndClose(&gitalypb.UserCommitFilesResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{CommitId: user.GetGlId() + "/" + user.GetGlUsername()},
	})
}

// gitConfigEchoServer reports the git_config_options it received, so that a
// test can tell whether a request passed the agent request guard.
type gitConfigEchoServer struct {
	gitalypb.UnimplementedSSHServiceServer
}

func (gitConfigEchoServer) SSHUploadPackWithSidechannel(_ context.Context, req *gitalypb.SSHUploadPackWithSidechannelRequest) (*gitalypb.SSHUploadPackWithSidechannelResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "options %q", req.GetGitConfigOptions())
}

func (gitConfigEchoServer) SSHReceivePack(stream gitalypb.SSHService_SSHReceivePackServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return status.Error(codes.Unimplemented, "end of stream")
			}
			return err
		}
		if err := stream.Send(&gitalypb.SSHReceivePackResponse{Stdout: []byte(strings.Join(req.GetGitConfigOptions(), ","))}); err != nil {
			return err
		}
	}
}

// agentConnection opens a connection that signs every request with the
// private key of the agent, as the SDK does.
func agentConnection(t *testing.T, address string, privateKey []byte) *grpc.ClientConn {
	t.Helper()

	signer, err := agentkey.ParsePrivateKey(privateKey)
	require.NoError(t, err)
	clientSigner := agentkey.NewSigner(signer)

	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(clientSigner.UnaryInterceptor()),
		grpc.WithChainStreamInterceptor(clientSigner.StreamInterceptor()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	return conn
}

// sharedConnection opens a connection that sends the shared token on every
// request.
func sharedConnection(t *testing.T, address, token string) *grpc.ClientConn {
	t.Helper()

	conn, err := grpc.NewClient(address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	return conn
}

// TestExternalServerAgentGitConfigOptions checks that an agent cannot pass Git
// configuration options, which could disable the hooks of a push or run
// arbitrary commands, while a shared-token caller still can.
func TestExternalServerAgentGitConfigOptions(t *testing.T) {
	t.Parallel()

	privateKey, publicKey := testhelper.AgentKeyPair(t)
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
		Auth: gitalycfgauth.Config{Token: "shared-secret"},
		Graft: config.Graft{
			Agents: []config.GraftAgent{{Name: "ci", PublicKey: publicKey}},
		},
	}))

	sf := server.NewGitalyServerFactory(
		cfg,
		testhelper.SharedLogger(t),
		backchannel.NewRegistry(),
		cache.New(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t)),
		nil,
		nil,
		server.TransactionMiddleware{},
		nil,
	)

	srv, err := sf.CreateExternal(false)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)

	gitalypb.RegisterSSHServiceServer(srv, gitConfigEchoServer{})

	listener, err := new(net.ListenConfig).Listen(t.Context(), starter.TCP, "localhost:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	agentConn := agentConnection(t, listener.Addr().String(), privateKey)
	sharedConn := sharedConnection(t, listener.Addr().String(), "shared-secret")

	repo := &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"}

	uploadPack := func(conn *grpc.ClientConn, ctx context.Context, options []string) error {
		_, err := gitalypb.NewSSHServiceClient(conn).SSHUploadPackWithSidechannel(ctx,
			&gitalypb.SSHUploadPackWithSidechannelRequest{Repository: repo, GitConfigOptions: options})
		return err
	}

	// receivePack sends a first message without options, then one with the
	// given options, and returns the error that ends the stream.
	receivePack := func(t *testing.T, conn *grpc.ClientConn, ctx context.Context, options []string) error {
		t.Helper()

		stream, err := gitalypb.NewSSHServiceClient(conn).SSHReceivePack(ctx)
		require.NoError(t, err)

		_ = stream.Send(&gitalypb.SSHReceivePackRequest{Repository: repo})
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		require.Empty(t, resp.GetStdout())

		_ = stream.Send(&gitalypb.SSHReceivePackRequest{GitConfigOptions: options})
		if err := stream.CloseSend(); err != nil {
			return err
		}
		for {
			if _, err := stream.Recv(); err != nil {
				return err
			}
		}
	}

	t.Run("agent cannot set options on a unary request", func(t *testing.T) {
		t.Parallel()
		testhelper.RequireGrpcCode(t, uploadPack(agentConn, testhelper.Context(t), []string{"uploadpack.packObjectsHook=touch /tmp/pwned"}), codes.PermissionDenied)
	})

	// SSH RPCs are not on the agent allowlist, so an agent is refused before the
	// request guard looks at the options. The guard itself is tested with the SSH
	// RPCs allowed, see TestAgentRequestGuard.
	t.Run("agent cannot call SSH RPCs even without options", func(t *testing.T) {
		t.Parallel()
		testhelper.RequireGrpcCode(t, uploadPack(agentConn, testhelper.Context(t), nil), codes.PermissionDenied)
	})

	t.Run("agent cannot set options in a later stream message", func(t *testing.T) {
		t.Parallel()
		testhelper.RequireGrpcCode(t, receivePack(t, agentConn, testhelper.Context(t), []string{"core.hooksPath=/var/empty"}), codes.PermissionDenied)
	})

	t.Run("shared token may set options", func(t *testing.T) {
		t.Parallel()

		err := uploadPack(sharedConn, testhelper.Context(t), []string{"transfer.hideRefs=refs/x"})
		testhelper.RequireGrpcCode(t, err, codes.Unimplemented)
		require.Contains(t, status.Convert(err).Message(), "transfer.hideRefs=refs/x")

		testhelper.RequireGrpcCode(t, receivePack(t, sharedConn, testhelper.Context(t), []string{"receive.maxInputSize=1"}), codes.Unimplemented)
	})
}

// TestExternalServerStandaloneIdentity checks through the external server that
// an agent cannot claim another actor in the User of a request, while a
// shared-token caller keeps the values it sent.
func TestExternalServerStandaloneIdentity(t *testing.T) {
	t.Parallel()

	privateKey, publicKey := testhelper.AgentKeyPair(t)
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
		Auth: gitalycfgauth.Config{Token: "shared-secret"},
		Graft: config.Graft{
			Standalone: true,
			Agents:     []config.GraftAgent{{Name: "ci", PublicKey: publicKey}},
		},
	}))

	sf := server.NewGitalyServerFactory(
		cfg,
		testhelper.SharedLogger(t),
		backchannel.NewRegistry(),
		cache.New(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t)),
		nil,
		nil,
		server.TransactionMiddleware{},
		nil,
	)

	srv, err := sf.CreateExternal(false)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)

	gitalypb.RegisterOperationServiceServer(srv, identityClaimServer{})

	listener, err := new(net.ListenConfig).Listen(t.Context(), starter.TCP, "localhost:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	agentConn := agentConnection(t, listener.Addr().String(), privateKey)
	sharedConn := sharedConnection(t, listener.Addr().String(), "shared-secret")

	request := func() *gitalypb.UserCreateBranchRequest {
		return &gitalypb.UserCreateBranchRequest{
			Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"},
			BranchName: []byte("main"),
			User:       &gitalypb.User{GlId: "admin", GlUsername: "root", Name: []byte("CI"), Email: []byte("ci@example.com")},
		}
	}

	t.Run("agent cannot claim another user", func(t *testing.T) {
		t.Parallel()

		resp, err := gitalypb.NewOperationServiceClient(agentConn).UserCreateBranch(testhelper.Context(t), request())
		require.NoError(t, err)
		require.Equal(t, "ci/ci", string(resp.GetBranch().GetName()))
	})

	t.Run("shared token keeps the client values", func(t *testing.T) {
		t.Parallel()

		resp, err := gitalypb.NewOperationServiceClient(sharedConn).UserCreateBranch(testhelper.Context(t), request())
		require.NoError(t, err)
		require.Equal(t, "admin/root", string(resp.GetBranch().GetName()))
	})

	commitFiles := func(t *testing.T, conn *grpc.ClientConn) string {
		t.Helper()

		stream, err := gitalypb.NewOperationServiceClient(conn).UserCommitFiles(testhelper.Context(t))
		require.NoError(t, err)
		require.NoError(t, stream.Send(&gitalypb.UserCommitFilesRequest{
			UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Header{
				Header: &gitalypb.UserCommitFilesRequestHeader{
					Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"},
					BranchName: []byte("main"),
					User:       &gitalypb.User{GlId: "admin", GlUsername: "root", Name: []byte("CI"), Email: []byte("ci@example.com")},
				},
			},
		}))

		resp, err := stream.CloseAndRecv()
		require.NoError(t, err)
		return resp.GetBranchUpdate().GetCommitId()
	}

	t.Run("agent cannot claim another user in a stream header", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, "ci/ci", commitFiles(t, agentConn))
	})

	t.Run("shared token keeps the stream header values", func(t *testing.T) {
		t.Parallel()

		require.Equal(t, "admin/root", commitFiles(t, sharedConn))
	})
}

func TestExternalServerAgentAuthentication(t *testing.T) {
	t.Parallel()

	privateKey, publicKey := testhelper.AgentKeyPair(t)
	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
		Auth: gitalycfgauth.Config{Token: "shared-secret"},
		Graft: config.Graft{
			Standalone: true,
			Agents:     []config.GraftAgent{{Name: "agent-a", PublicKey: publicKey}},
		},
	}))

	sf := server.NewGitalyServerFactory(
		cfg,
		testhelper.SharedLogger(t),
		backchannel.NewRegistry(),
		cache.New(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t)),
		nil,
		nil,
		server.TransactionMiddleware{},
		nil,
	)

	srv, err := sf.CreateExternal(false)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)

	healthpb.RegisterHealthServer(srv, auth.UnauthenticatedHealthService{HealthServer: health.NewServer()})
	gitalypb.RegisterServerServiceServer(srv, identityEchoServer{})
	gitalypb.RegisterHookServiceServer(srv, gitalypb.UnimplementedHookServiceServer{})

	listener, err := new(net.ListenConfig).Listen(t.Context(), starter.TCP, "localhost:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	unsignedConn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, unsignedConn.Close()) })

	agentConn := agentConnection(t, listener.Addr().String(), privateKey)
	sharedConn := sharedConnection(t, listener.Addr().String(), "shared-secret")

	t.Run("health check needs no token", func(t *testing.T) {
		t.Parallel()

		resp, err := healthpb.NewHealthClient(unsignedConn).Check(testhelper.Context(t), &healthpb.HealthCheckRequest{})
		require.NoError(t, err)
		require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())
	})

	t.Run("the signature of the agent sets the agent identity", func(t *testing.T) {
		t.Parallel()

		resp, err := gitalypb.NewServerServiceClient(agentConn).ServerInfo(testhelper.Context(t), &gitalypb.ServerInfoRequest{})
		require.NoError(t, err)
		require.Equal(t, "agent-a/agent", resp.GetServerVersion())
	})

	t.Run("shared token sets the shared identity", func(t *testing.T) {
		t.Parallel()

		resp, err := gitalypb.NewServerServiceClient(sharedConn).ServerInfo(testhelper.Context(t), &gitalypb.ServerInfoRequest{})
		require.NoError(t, err)
		require.Equal(t, "shared/shared_token", resp.GetServerVersion())
	})

	t.Run("missing token is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := gitalypb.NewServerServiceClient(unsignedConn).ServerInfo(testhelper.Context(t), &gitalypb.ServerInfoRequest{})
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("wrong token is rejected", func(t *testing.T) {
		t.Parallel()

		ctx := metadata.AppendToOutgoingContext(testhelper.Context(t), "authorization", "Bearer nope")
		_, err := gitalypb.NewServerServiceClient(unsignedConn).ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	// HookService trusts the actor carried in its requests' hooks payload, so an
	// agent calling it could act as another actor.
	referenceTransactionHook := func(t *testing.T, conn *grpc.ClientConn) error {
		t.Helper()

		stream, err := gitalypb.NewHookServiceClient(conn).ReferenceTransactionHook(testhelper.Context(t))
		require.NoError(t, err)
		// Send fails with io.EOF when the server already rejected the stream; the
		// status is returned by Recv.
		_ = stream.Send(&gitalypb.ReferenceTransactionHookRequest{
			Repository:           &gitalypb.Repository{StorageName: "default", RelativePath: "repo.git"},
			EnvironmentVariables: []string{"GITALY_HOOKS_PAYLOAD=forged"},
			State:                gitalypb.ReferenceTransactionHookRequest_COMMITTED,
		})
		require.NoError(t, stream.CloseSend())
		_, err = stream.Recv()
		return err
	}

	t.Run("agent cannot call HookService", func(t *testing.T) {
		t.Parallel()

		_, err := gitalypb.NewHookServiceClient(agentConn).PackObjectsHookWithSidechannel(testhelper.Context(t), &gitalypb.PackObjectsHookWithSidechannelRequest{})
		testhelper.RequireGrpcCode(t, err, codes.PermissionDenied)

		testhelper.RequireGrpcCode(t, referenceTransactionHook(t, agentConn), codes.PermissionDenied)
	})

	t.Run("shared token may call HookService", func(t *testing.T) {
		t.Parallel()

		// The stub server does not implement the RPCs, so passing authentication
		// yields Unimplemented.
		_, err := gitalypb.NewHookServiceClient(sharedConn).PackObjectsHookWithSidechannel(testhelper.Context(t), &gitalypb.PackObjectsHookWithSidechannelRequest{})
		testhelper.RequireGrpcCode(t, err, codes.Unimplemented)

		testhelper.RequireGrpcCode(t, referenceTransactionHook(t, sharedConn), codes.Unimplemented)
	})
}

func TestExternalServerWithoutAgentsKeepsUpstreamAuthentication(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{
		Auth: gitalycfgauth.Config{Token: "shared-secret"},
	}))

	sf := server.NewGitalyServerFactory(
		cfg,
		testhelper.SharedLogger(t),
		backchannel.NewRegistry(),
		cache.New(cfg, config.NewLocator(cfg), testhelper.SharedLogger(t)),
		nil,
		nil,
		server.TransactionMiddleware{},
		nil,
	)

	srv, err := sf.CreateExternal(false)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)

	gitalypb.RegisterServerServiceServer(srv, identityEchoServer{})

	listener, err := new(net.ListenConfig).Listen(t.Context(), starter.TCP, "localhost:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(listener) }()

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })

	// Without agents a plain token is not a credential: a Gitaly server only
	// accepts v2 HMAC tokens.
	ctx := metadata.AppendToOutgoingContext(testhelper.Context(t), "authorization", "Bearer agent-a-secret")
	_, err = gitalypb.NewServerServiceClient(conn).ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	md, err := gitalyauth.RPCCredentialsV2("shared-secret").GetRequestMetadata(testhelper.Context(t))
	require.NoError(t, err)
	ctx = metadata.AppendToOutgoingContext(testhelper.Context(t), "authorization", md["authorization"])

	resp, err := gitalypb.NewServerServiceClient(conn).ServerInfo(ctx, &gitalypb.ServerInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "shared/shared_token", resp.GetServerVersion())
}
