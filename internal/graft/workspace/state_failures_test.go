package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// breakMeta makes the metadata directory of the workspace read-only, so that
// the workspace can be read but every write of the state fails and the command
// has to report it.
func breakMeta(t *testing.T, root string) {
	t.Helper()

	meta := filepath.Join(root, metaDirName)
	require.NoError(t, os.Chmod(meta, 0o555))
	t.Cleanup(func() { _ = os.Chmod(meta, 0o755) })
}

// TestEveryWriteReportsTheStateItCannotSave drives the writes of a workspace
// whose metadata directory cannot be used: a commit, a land, an update, a
// sparse set, an abort and a revert each report the failure instead of
// reporting a success the next command cannot read back.
func TestEveryWriteReportsTheStateItCannotSave(t *testing.T) {
	t.Parallel()

	t.Run("commit", func(t *testing.T) {
		ws := cloneTestWorkspace(t, faultBackendFor(t, func(*faultBackend) {}))
		writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")
		breakMeta(t, ws.Root())

		_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
		require.Error(t, err)
	})

	t.Run("land", func(t *testing.T) {
		ws := cloneTestWorkspace(t, faultBackendFor(t, func(*faultBackend) {}))
		writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")
		_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
		require.NoError(t, err)
		breakMeta(t, ws.Root())

		_, err = ws.Land(context.Background(), "main", "", Signature{Name: "Dev"})
		require.ErrorContains(t, err, "saving the workspace state failed")
	})

	t.Run("update", func(t *testing.T) {
		b := faultBackendFor(t, func(*faultBackend) {})
		ws := cloneTestWorkspace(t, b)
		breakMeta(t, ws.Root())

		_, err := ws.Update(context.Background())
		require.Error(t, err)
	})

	t.Run("sparse", func(t *testing.T) {
		ws := cloneTestWorkspace(t, faultBackendFor(t, func(*faultBackend) {}))
		breakMeta(t, ws.Root())

		require.Error(t, ws.Sparse(context.Background(), []string{"a"}))
	})

	t.Run("abort", func(t *testing.T) {
		ws := cloneTestWorkspace(t, faultBackendFor(t, func(*faultBackend) {}))
		writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")
		_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
		require.NoError(t, err)
		breakMeta(t, ws.Root())

		require.Error(t, ws.Abort(context.Background()))
	})

	t.Run("revert", func(t *testing.T) {
		ws := cloneTestWorkspace(t, faultBackendFor(t, func(*faultBackend) {}))
		writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")
		breakMeta(t, ws.Root())

		require.Error(t, ws.Revert(context.Background(), nil))
	})
}

// TestCommitReportsAFailedRefreshAfterALand covers the land that succeeded and
// the refresh that did not: the commit is on the branch, so the error says so
// instead of offering a retry that would land it twice.
func TestCommitReportsAFailedRefreshAfterALand(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	// The land works, the tree the refresh reads does not.
	b.failListTree = true
	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x", LandBranch: "main"})
	require.ErrorContains(t, err, "committed ")
	require.ErrorContains(t, err, "the server refused the call")
}

// TestSparseFinishesAStartedRefresh covers the sparse set as the way out of a
// refresh that keeps failing: the pending commit has landed, so the sparse set
// moves the workspace to the landed commit and clears the pending commit.
func TestSparseFinishesAStartedRefresh(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	// The land works, the refresh after it does not: the commit is on the
	// branch and stays pending in the workspace.
	b.failListTree = true
	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x", LandBranch: "main"})
	require.Error(t, err)

	// A sparse set that leaves out the whole tree finishes the refresh.
	b.failListTree = false
	require.NoError(t, ws.Sparse(context.Background(), []string{"nothing"}))
	require.Nil(t, ws.Summary().Pending)
}
