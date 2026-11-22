package lifecycle_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/lifecycle"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// TestGuardRefusesRemoveRepository: the removal of a repository is refused
// with FailedPrecondition, and the handler never runs. Every other RPC is
// passed through (doc/graft/PLAN13.md).
func TestGuardRefusesRemoveRepository(t *testing.T) {
	t.Parallel()

	guard := lifecycle.New(testhelper.SharedLogger(t))
	ctx := testhelper.Context(t)

	called := false
	handler := func(context.Context, any) (any, error) {
		called = true
		return "ok", nil
	}

	_, err := guard.UnaryInterceptor()(ctx, struct{}{},
		&grpc.UnaryServerInfo{FullMethod: "/gitaly.RepositoryService/RemoveRepository"}, handler)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "archive it instead")
	require.False(t, called, "the handler of a removal never runs")

	for _, method := range []string{
		"/gitaly.RepositoryService/CreateRepository",
		"/gitaly.StorageCoreService/ArchiveRepository",
		"/gitaly.StorageCoreService/PurgeRepository",
		"/gitaly.OperationService/UserLand",
	} {
		got, err := guard.UnaryInterceptor()(ctx, struct{}{}, &grpc.UnaryServerInfo{FullMethod: method}, handler)
		require.NoError(t, err, method)
		require.Equal(t, "ok", got, method)
	}
}

// TestGuardPassesStreams: the removal is not a streaming RPC, so the stream
// interceptor passes every call through.
func TestGuardPassesStreams(t *testing.T) {
	t.Parallel()

	guard := lifecycle.New(testhelper.SharedLogger(t))
	called := false
	handler := func(any, grpc.ServerStream) error {
		called = true
		return nil
	}
	require.NoError(t, guard.StreamInterceptor()(nil, nil,
		&grpc.StreamServerInfo{FullMethod: "/gitaly.OperationService/UserCommitFiles"}, handler))
	require.True(t, called)
}
