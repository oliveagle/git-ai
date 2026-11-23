package verify

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// refusingRepo is a repository of a server that answers every RPC with
// Unimplemented: the backend of the verifier must hand every refusal back to
// the caller instead of reading it as an empty answer.
func refusingRepo(t *testing.T) *sdk.Repo {
	t.Helper()

	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := grpc.NewServer()
	t.Cleanup(srv.Stop)
	go func() { _ = srv.Serve(listener) }()

	client, err := sdk.Dial(t.Context(), "tcp://"+listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return client.Repo("project.git")
}

// TestTheSDKBackendReportsWhatTheServerRefuses covers the read methods of the
// backend the verifier runs on: a server that refuses a call makes the method
// fail, and a NOT_FOUND answer is translated for the workspace.
func TestTheSDKBackendReportsWhatTheServerRefuses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	backend := &sdkBackend{repo: refusingRepo(t)}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"resolve revision", func() error { _, err := backend.ResolveRevision(ctx, "main"); return err }},
		{"list tree", func() error { _, err := backend.ListTree(ctx, "main", "", false); return err }},
		{"get objects", func() error { _, err := backend.GetObjects(ctx, []string{"0000000000000000000000000000000000000001"}); return err }},
		{"read file", func() error { _, err := backend.ReadFile(ctx, "main", "a.txt"); return err }},
		{"changesets", func() error { _, err := backend.Changesets(ctx, 0, 10); return err }},
		{"blame", func() error { _, err := backend.Blame(ctx, "main", "a.txt"); return err }},
		{"path history", func() error { _, err := backend.PathHistory(ctx, "main", "a.txt", 10); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Error(t, tc.call())
		})
	}
}

// TestTheSDKBackendWatchesChangesetsOfARefusingServer covers the watch: the
// refusal arrives on the error channel, and both channels end.
func TestTheSDKBackendWatchesChangesetsOfARefusingServer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	backend := &sdkBackend{repo: refusingRepo(t)}
	changes, errs := backend.WatchChangesets(ctx, 0)

	var sawErr bool
	for err := range errs {
		if err != nil {
			sawErr = true
		}
	}
	for range changes {
	}
	require.True(t, sawErr, "the refusal of the watch is reported")
}

// TestMapErrorTranslatesWhatTheWorkspaceKnows covers the translation of the
// errors the workspace reacts to: a missing object is ErrNotFound, everything
// else is handed back unchanged.
func TestMapErrorTranslatesWhatTheWorkspaceKnows(t *testing.T) {
	t.Parallel()

	require.NoError(t, mapError(nil))

	err := mapError(status.Error(codes.NotFound, "no such commit"))
	require.ErrorIs(t, err, workspace.ErrNotFound)

	other := errors.New("something else")
	require.Equal(t, other, mapError(other))
}

// TestObjectTypeAndClamp cover the two conversions of the backend: an object
// type that is neither blob, tree nor commit falls back to its own name, and a
// count is clamped to the range of the protocol.
func TestObjectTypeAndClamp(t *testing.T) {
	t.Parallel()

	require.Equal(t, workspace.ObjectBlob, objectType(sdk.ObjectBlob))
	require.Equal(t, workspace.ObjectTree, objectType(sdk.ObjectTree))
	require.Equal(t, workspace.ObjectCommit, objectType(sdk.ObjectCommit))
	require.Equal(t, workspace.ObjectType("tag"), objectType(gitalypb.ObjectType_TAG))

	require.EqualValues(t, 0, clampUint32(-1))
	require.EqualValues(t, 0, clampUint32(0))
	require.EqualValues(t, 10, clampUint32(10))
}
