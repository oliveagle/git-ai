package workspace

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCloneAtACommitMaterializesItsTree checks the workspace a CI needs for a
// merge queue: the tree of the commit a dry-run land named, which no branch
// points at, materialized whole.
func TestCloneAtACommitMaterializesItsTree(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt", "b/x.txt"}, []string{"a", "bx"})
	tip, err := b.ResolveRevision(context.Background(), "main")
	require.NoError(t, err)

	root := filepath.Join(t.TempDir(), "ws")
	ws, err := Clone(context.Background(), root, b, CloneOptions{
		Address: "memory",
		Storage: "default",
		Repo:    "repo",
		At:      tip,
	})
	require.NoError(t, err)

	// The whole tree, and no branch: the workspace is at the commit.
	require.Equal(t, "a", readWorkspaceFile(t, root, "a.txt"))
	require.Equal(t, "bx", readWorkspaceFile(t, root, "b/x.txt"))
	require.Equal(t, tip, ws.Summary().Base)
	require.Empty(t, ws.Summary().Branch)

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
}

// TestCloneAtACommitNeedsABranchToLand checks the errors of a workspace at a
// commit: it has no branch to update to and none to land on unless the caller
// names one, and a land that names one attaches the workspace to it.
func TestCloneAtACommitNeedsABranchToLand(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	tip, err := b.ResolveRevision(context.Background(), "main")
	require.NoError(t, err)

	root := filepath.Join(t.TempDir(), "ws")
	ws, err := Clone(context.Background(), root, b, CloneOptions{At: tip})
	require.NoError(t, err)

	_, err = ws.Update(context.Background())
	require.ErrorContains(t, err, "not on a branch")

	writeWorkspaceFile(t, root, "c.txt", "c")
	_, err = ws.Commit(context.Background(), CommitRequest{
		Message: "add c",
		Author:  Signature{Name: "CI", Email: "ci@example.com"},
	})
	require.NoError(t, err)

	_, err = ws.Land(context.Background(), "", StrategyRebase, Signature{Name: "CI", Email: "ci@example.com"})
	require.ErrorContains(t, err, "name the branch to land on")

	// A land that names the branch works, and the workspace then tracks it.
	res, err := ws.Land(context.Background(), "main", StrategyRebase, Signature{Name: "CI", Email: "ci@example.com"})
	require.NoError(t, err)
	require.NotEmpty(t, res.Commit)
	require.Equal(t, "c", readRemote(t, b, "main", "c.txt"))
	require.Equal(t, "main", ws.Summary().Branch)
}

// TestCloneRejectsABranchAndACommitTogether checks the command line of the
// workspace: a tree at a commit is not the tip of a branch.
func TestCloneRejectsABranchAndACommitTogether(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	tip, err := b.ResolveRevision(context.Background(), "main")
	require.NoError(t, err)

	_, err = Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), b, CloneOptions{Branch: "main", At: tip})
	require.ErrorContains(t, err, "either a branch or a commit")
}
