package clientver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/testhelper"
)

// reported returns a context as a client would send it.
func reported(t *testing.T, version string) context.Context {
	t.Helper()
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(MetadataKey, version))
}

// TestReported covers the two ways a request arrives without a version: no
// metadata at all, and metadata that carries other keys.
func TestReported(t *testing.T) {
	t.Parallel()

	_, ok := Reported(context.Background())
	require.False(t, ok)

	_, ok = Reported(metadata.NewIncomingContext(context.Background(), metadata.Pairs("other", "x")))
	require.False(t, ok)

	raw, ok := Reported(reported(t, "0.9.1"))
	require.True(t, ok)
	require.Equal(t, "0.9.1", raw)
}

// TestUnaryServerInterceptorRefusesOldClients covers the gate: a client below
// the minimum and a client that reports nothing are refused with the command
// that upgrades them, and a client at or above the minimum is answered.
func TestUnaryServerInterceptorRefusesOldClients(t *testing.T) {
	t.Parallel()

	min, ok := Parse("0.9.1")
	require.True(t, ok)
	interceptor := UnaryServerInterceptor(min, testhelper.NewLogger(t))

	handled := false
	handler := func(context.Context, any) (any, error) {
		handled = true
		return "answered", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/gitaly.RepositoryService/RepositorySize"}

	// A client from before the contract reports nothing at all.
	_, err := interceptor(context.Background(), nil, info, handler)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "0.9.1")
	require.Contains(t, err.Error(), "graft upgrade")
	require.False(t, handled)

	// A client below the minimum is refused, and named.
	_, err = interceptor(reported(t, "0.9.0"), nil, info, handler)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "0.9.0")
	require.False(t, handled)

	// At the minimum and above it, the call is answered.
	for _, v := range []string{"0.9.1", "0.9.1-2-g1a2b3c4", "0.10.0", "v1.0.0"} {
		handled = false
		got, err := interceptor(reported(t, v), nil, info, handler)
		require.NoError(t, err, v)
		require.Equal(t, "answered", got, v)
		require.True(t, handled, v)
	}
}

// TestUnaryServerInterceptorAllowsAnUnversionedBuild covers a build of this
// tree without the release ldflags: it reports "unknown", which cannot be
// compared. Refusing it would refuse the server's own tools, so it is answered
// and the reported version is in the log.
func TestUnaryServerInterceptorAllowsAnUnversionedBuild(t *testing.T) {
	t.Parallel()

	min, ok := Parse("0.9.1")
	require.True(t, ok)
	interceptor := UnaryServerInterceptor(min, testhelper.NewLogger(t))

	got, err := interceptor(reported(t, unreportedVersion), nil,
		&grpc.UnaryServerInfo{FullMethod: "/gitaly.RepositoryService/RepositorySize"},
		func(context.Context, any) (any, error) { return "answered", nil })
	require.NoError(t, err)
	require.Equal(t, "answered", got)
}

// TestServerInterceptorAnswersTheExemptMethods covers what a server must
// answer whatever the client reports: an orchestrator's health probe and the
// version report a client reads before it gives up.
func TestServerInterceptorAnswersTheExemptMethods(t *testing.T) {
	t.Parallel()

	min, ok := Parse("0.9.1")
	require.True(t, ok)
	logger := testhelper.NewLogger(t)

	unary := UnaryServerInterceptor(min, logger)
	for _, method := range []string{"/grpc.health.v1.Health/Check", "/gitaly.ServerService/ServerInfo"} {
		got, err := unary(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: method},
			func(context.Context, any) (any, error) { return "answered", nil })
		require.NoError(t, err, method)
		require.Equal(t, "answered", got, method)
	}

	stream := StreamServerInterceptor(min, logger)
	err := stream(nil, &testServerStream{ctx: context.Background()},
		&grpc.StreamServerInfo{FullMethod: "/grpc.health.v1.Health/Watch"},
		func(any, grpc.ServerStream) error { return nil })
	require.NoError(t, err)
}

// TestStreamServerInterceptorRefusesAnOldClient covers the same gate on a
// stream, which is how the object and pack RPCs travel.
func TestStreamServerInterceptorRefusesAnOldClient(t *testing.T) {
	t.Parallel()

	min, ok := Parse("0.9.1")
	require.True(t, ok)
	interceptor := StreamServerInterceptor(min, testhelper.NewLogger(t))

	handled := false
	handler := func(any, grpc.ServerStream) error {
		handled = true
		return nil
	}
	info := &grpc.StreamServerInfo{FullMethod: "/gitaly.SSHService/SSHUploadPack"}

	err := interceptor(nil, &testServerStream{ctx: context.Background()}, info, handler)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.False(t, handled)

	err = interceptor(nil, &testServerStream{ctx: reported(t, "0.9.1")}, info, handler)
	require.NoError(t, err)
	require.True(t, handled)
}

// TestClientInterceptorsReportTheVersion covers the other half of the
// contract: every request of this build carries its version, and a build
// without one carries the marker that says it cannot be compared.
func TestClientInterceptorsReportTheVersion(t *testing.T) {
	t.Parallel()

	want := unreportedVersion // the test binary is built without the release ldflags

	var got []string
	err := UnaryClientInterceptor()(context.Background(), "/m", nil, nil, nil,
		func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
			md, ok := metadata.FromOutgoingContext(ctx)
			require.True(t, ok)
			got = md.Get(MetadataKey)
			return nil
		})
	require.NoError(t, err)
	require.Equal(t, []string{want}, got)

	_, err = StreamClientInterceptor()(context.Background(), &grpc.StreamDesc{}, nil, "/m",
		func(ctx context.Context, _ *grpc.StreamDesc, _ *grpc.ClientConn, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
			md, ok := metadata.FromOutgoingContext(ctx)
			require.True(t, ok)
			got = md.Get(MetadataKey)
			return nil, nil
		})
	require.NoError(t, err)
	require.Equal(t, []string{want}, got)
}

// testServerStream is the server side of a stream: only its context is read.
type testServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *testServerStream) Context() context.Context { return s.ctx }
