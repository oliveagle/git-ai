package server

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/feder-cr/git-ai/internal/bootstrap/starter"
	"github.com/feder-cr/git-ai/internal/cache"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// echoService is the empty interface the echo service is registered against:
// the handler below answers, and the gate runs before it.
type echoService interface{}

type echoServer struct{}

// echoDesc is a unary service whose handler answers with what it was sent, so
// that reaching it (or not) is what the test observes.
var echoDesc = grpc.ServiceDesc{
	ServiceName: "graft.Echo",
	HandlerType: (*echoService)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Call",
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			req := &emptypb.Empty{}
			if err := dec(req); err != nil {
				return nil, err
			}
			if interceptor == nil {
				return req, nil
			}
			info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/graft.Echo/Call"}
			return interceptor(ctx, req, info, func(context.Context, any) (any, error) { return req, nil })
		},
	}},
}

// TestClientVersionGate covers the version contract on the wire: on the
// external API a client that reports nothing is refused with
// FailedPrecondition, a client of this build is answered, and the health probe
// an orchestrator uses is answered whatever the client is. The internal
// socket, which carries this process's own hook calls, has no gate at all.
func TestClientVersionGate(t *testing.T) {
	ctx := testhelper.Context(t)
	logger := testhelper.SharedLogger(t)

	cfg := testcfg.Build(t)
	cfg.Graft.MinClientVersion = "0.9.0"

	factory := NewGitalyServerFactory(
		cfg,
		logger,
		backchannel.NewRegistry(),
		cache.New(cfg, config.NewLocator(cfg), logger),
		nil,
		nil,
		TransactionMiddleware{},
		nil,
	)
	t.Cleanup(factory.Stop)

	serve := func(t *testing.T, srv *grpc.Server) string {
		t.Helper()

		srv.RegisterService(&echoDesc, echoServer{})
		healthpb.RegisterHealthServer(srv, health.NewServer())

		listener, err := (&net.ListenConfig{}).Listen(ctx, starter.TCP, "localhost:0")
		require.NoError(t, err)
		go func() { _ = srv.Serve(listener) }()

		endpoint, err := starter.ComposeEndpoint(starter.TCP, listener.Addr().String())
		require.NoError(t, err)
		return endpoint
	}

	// An old client: it dials without the version metadata, as every client
	// built before the contract does.
	oldDial := func(t *testing.T, endpoint string) *grpc.ClientConn {
		t.Helper()

		conn, err := grpc.NewClient(strings.TrimPrefix(endpoint, "tcp://"),
			grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}

	external, err := factory.CreateExternal(false)
	require.NoError(t, err)
	t.Cleanup(external.Stop)
	externalEndpoint := serve(t, external)

	old := oldDial(t, externalEndpoint)
	err = old.Invoke(ctx, "/graft.Echo/Call", &emptypb.Empty{}, &emptypb.Empty{})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "0.9.0")
	require.Contains(t, err.Error(), "graft upgrade")

	// The health probe is answered, so an orchestrator still sees the process.
	healthClient := healthpb.NewHealthClient(old)
	resp, err := healthClient.Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthpb.HealthCheckResponse_SERVING, resp.GetStatus())

	// A client of this build reports its version ("unknown" without the release
	// ldflags) and is answered.
	newConn, err := client.New(ctx, externalEndpoint)
	require.NoError(t, err)
	t.Cleanup(func() { _ = newConn.Close() })
	require.NoError(t, newConn.Invoke(ctx, "/graft.Echo/Call", &emptypb.Empty{}, &emptypb.Empty{}))

	// The internal socket carries this process's own hook calls: no gate.
	internal, err := factory.CreateInternal()
	require.NoError(t, err)
	t.Cleanup(internal.Stop)
	internalEndpoint := serve(t, internal)
	require.NoError(t, oldDial(t, internalEndpoint).Invoke(ctx, "/graft.Echo/Call", &emptypb.Empty{}, &emptypb.Empty{}))
}
