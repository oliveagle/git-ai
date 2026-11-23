package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// TestToSDKActions maps every action of a workspace to the action of the sdk,
// and refuses an action it does not know.
func TestToSDKActions(t *testing.T) {
	t.Parallel()

	actions, err := toSDKActions([]workspace.Action{
		{Kind: workspace.ActionPut, Path: "a.txt", Content: []byte("a"), Executable: true, Overwrite: true},
		{Kind: workspace.ActionDelete, Path: "b.txt"},
		{Kind: workspace.ActionMove, FromPath: "c.txt", Path: "d.txt"},
		{Kind: workspace.ActionChmod, Path: "e.txt", Executable: true},
	})
	require.NoError(t, err)
	require.Len(t, actions, 4)
	require.Equal(t, sdk.Put{Path: "a.txt", Content: []byte("a"), Executable: true, Overwrite: true}, actions[0])
	require.Equal(t, sdk.Delete{Path: "b.txt"}, actions[1])
	require.Equal(t, sdk.Move{From: "c.txt", To: "d.txt"}, actions[2])
	require.Equal(t, sdk.Chmod{Path: "e.txt", Executable: true}, actions[3])

	_, err = toSDKActions([]workspace.Action{{Kind: workspace.ActionKind("unknown")}})
	require.ErrorContains(t, err, "unknown action")
}

// TestLandStrategy maps every strategy of a workspace to the strategy of the
// sdk, and refuses one it does not know.
func TestLandStrategy(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]sdk.LandStrategy{
		"":                       sdk.LandRebase,
		workspace.StrategyRebase: sdk.LandRebase,
		workspace.StrategyMerge:  sdk.LandMerge,
		workspace.StrategyFFOnly: sdk.LandFastForwardOnly,
	} {
		got, err := landStrategy(name)
		require.NoError(t, err, name)
		require.Equal(t, want, got, name)
	}

	_, err := landStrategy("cherry-pick")
	require.ErrorContains(t, err, "unknown strategy")
}

// TestObjectType maps the types of the sdk to the types of a workspace, and
// keeps the name of a type it does not know.
func TestObjectType(t *testing.T) {
	t.Parallel()

	require.Equal(t, workspace.ObjectBlob, objectType(sdk.ObjectBlob))
	require.Equal(t, workspace.ObjectTree, objectType(sdk.ObjectTree))
	require.Equal(t, workspace.ObjectCommit, objectType(sdk.ObjectCommit))
	require.Equal(t, workspace.ObjectType("tag"), objectType(gitalypb.ObjectType_TAG))
}

// TestClosers closes everything a command opened, and leaves a resource that
// was registered without a closer list alone.
func TestClosers(t *testing.T) {
	t.Parallel()

	first := &fakeCloser{}
	second := &fakeCloser{}
	ctx, closeAll := withClosers(context.Background())
	registerCloser(ctx, first)
	registerCloser(ctx, second)
	closeAll()
	require.Equal(t, 1, first.closes)
	require.Equal(t, 1, second.closes)

	// A resource registered without a closer list stays open: the command
	// that opened it is the one that closes it.
	orphan := &fakeCloser{}
	registerCloser(context.Background(), orphan)
	require.Equal(t, 0, orphan.closes)
}

// fakeCloser counts how often it was closed.
type fakeCloser struct {
	closes int
	err    error
}

func (c *fakeCloser) Close() error {
	c.closes++
	return c.err
}

// TestOpenSDKBackendNeedsAnAddressAndARepository refuses a backend that names
// no server or no repository, before a connection is made.
func TestOpenSDKBackendNeedsAnAddressAndARepository(t *testing.T) {
	t.Parallel()

	_, err := openSDKBackend(context.Background(), target{})
	require.ErrorContains(t, err, "no server address")

	_, err = openSDKBackend(context.Background(), target{Address: "127.0.0.1:1"})
	require.ErrorContains(t, err, "does not name a repository")
}

// TestOpenSDKBackendReportsADialFailure refuses a backend whose server cannot
// be reached.
func TestOpenSDKBackendReportsADialFailure(t *testing.T) {
	t.Parallel()

	_, err := openSDKBackend(context.Background(), target{Address: "127.0.0.1:1", Repo: "shop.git"})
	require.Error(t, err)
}
