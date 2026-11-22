package hook

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestHookService_referenceTransactionWarning covers how a warning of the hook
// reaches the pusher: the transaction goes through, and the message comes back
// as the stderr of the reference-transaction hook, where git prints it.
func TestHookService_referenceTransactionWarning(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	message := "the mirror codeup of the repository is 12 commits behind the branch main"

	manager := hook.NewMockManager(t, hook.NopPreReceive, hook.NopPostReceive, hook.NopUpdate,
		func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
			hook.TransactionMessagesFrom(ctx).Add(message)
			return nil
		},
	)
	cfg.SocketPath = runHooksServer(t, cfg, nil, testserver.WithHookManager(manager))
	repo, _ := gittest.CreateRepository(t, ctx, cfg)
	client, conn := newHooksClient(t, cfg.SocketPath)
	defer conn.Close()

	stream, err := client.ReferenceTransactionHook(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.ReferenceTransactionHookRequest{
		Repository: repo, State: gitalypb.ReferenceTransactionHookRequest_PREPARED,
	}))
	require.NoError(t, stream.CloseSend())

	var stderr []byte
	var exit *gitalypb.ExitStatus
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		stderr = append(stderr, resp.GetStderr()...)
		if resp.GetExitStatus() != nil {
			exit = resp.GetExitStatus()
		}
	}

	require.Equal(t, "warning: "+message+"\n", string(stderr))
	require.Equal(t, int32(0), exit.GetValue())
}

// TestHookService_referenceTransactionRefusal covers the other half of what a
// policy says to the pusher: a refusal aborts the update and its reason is the
// stderr of the hook, where git prints it.
func TestHookService_referenceTransactionRefusal(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	reason := "the repository shop.git has no mirror target: add one with \"graft mirror queue add\""

	manager := hook.NewMockManager(t, hook.NopPreReceive, hook.NopPostReceive, hook.NopUpdate,
		func(t *testing.T, ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
			return structerr.NewPermissionDenied("%s", reason)
		},
	)
	cfg.SocketPath = runHooksServer(t, cfg, nil, testserver.WithHookManager(manager))
	repo, _ := gittest.CreateRepository(t, ctx, cfg)
	client, conn := newHooksClient(t, cfg.SocketPath)
	defer conn.Close()

	stream, err := client.ReferenceTransactionHook(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.ReferenceTransactionHookRequest{
		Repository: repo, State: gitalypb.ReferenceTransactionHookRequest_PREPARED,
	}))
	require.NoError(t, stream.CloseSend())

	var stderr []byte
	var exit *gitalypb.ExitStatus
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		stderr = append(stderr, resp.GetStderr()...)
		if resp.GetExitStatus() != nil {
			exit = resp.GetExitStatus()
		}
	}

	require.Equal(t, reason+"\n", string(stderr))
	require.Equal(t, int32(1), exit.GetValue())
}
