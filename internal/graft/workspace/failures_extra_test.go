package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// corruptState writes a state file the workspace cannot read.
func corruptState(t *testing.T, root string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Join(root, metaDirName), 0o755))
	require.NoError(t, os.WriteFile(statePath(root), []byte("{not json"), 0o600))
}

// TestOpenRefusesWhatItCannotLoad covers the workspace it cannot open: the
// backend that is not there and the state that cannot be read.
func TestOpenRefusesWhatItCannotLoad(t *testing.T) {
	t.Parallel()

	_, err := Open(t.TempDir(), nil)
	require.ErrorContains(t, err, "nil backend")

	root := t.TempDir()
	corruptState(t, root)
	_, err = Open(root, NewMemoryBackend())
	require.ErrorContains(t, err, "parse")

	_, err = Open(filepath.Join(t.TempDir(), "absent"), NewMemoryBackend())
	require.ErrorIs(t, err, ErrNotWorkspace)
}

// TestTheCommandsRefuseTheStateTheyCannotLoad covers the command that reads
// the state again under the lock, and finds it damaged.
func TestTheCommandsRefuseTheStateTheyCannotLoad(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	corruptState(t, ws.Root())

	_, err := ws.Status()
	require.Error(t, err)

	_, err = ws.Commit(context.Background(), CommitRequest{Message: "change"})
	require.Error(t, err)

	_, err = ws.Update(context.Background())
	require.Error(t, err)

	err = ws.Abort(context.Background())
	require.Error(t, err)

	err = ws.Revert(context.Background(), nil)
	require.Error(t, err)
}

// TestCloneUsesMainWhenNoBranchIsNamed covers the clone that takes the
// default branch of a repository.
func TestCloneUsesMainWhenNoBranchIsNamed(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
	ws, err := Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), b, CloneOptions{})
	require.NoError(t, err)
	require.Equal(t, "main", ws.state.Branch)
}

// TestCloneRefusesADirectoryItCannotRead covers the clone into a path that is
// a file, and the clone into a directory that holds something already.
func TestCloneRefusesADirectoryItCannotRead(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	_, err := Clone(context.Background(), file, b, CloneOptions{Branch: "main"})
	require.Error(t, err)
}

// TestCommitRefusesWhatItCannotRead covers the file that is not a regular
// file and the file it cannot read.
func TestCommitRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	t.Run("a symbolic link", func(t *testing.T) {
		b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
		ws := cloneTestWorkspace(t, b)
		require.NoError(t, os.Symlink(filepath.Join(ws.Root(), "a", "x.txt"), filepath.Join(ws.Root(), "a", "link.txt")))

		_, err := ws.Commit(context.Background(), CommitRequest{Message: "change"})
		require.ErrorContains(t, err, "symbolic link")
	})

	t.Run("a file it cannot read", func(t *testing.T) {
		b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
		ws := cloneTestWorkspace(t, b)
		path := filepath.Join(ws.Root(), "a", "x.txt")
		require.NoError(t, os.Chmod(path, 0o000))
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

		_, err := ws.Commit(context.Background(), CommitRequest{Message: "change"})
		require.Error(t, err)
	})
}

// TestStatusReportsTheFileItCannotRead covers the directory it cannot walk.
func TestStatusReportsTheFileItCannotRead(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	require.NoError(t, os.Chmod(filepath.Join(ws.Root(), "a"), 0o000))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(ws.Root(), "a"), 0o755) })

	_, err := ws.Status()
	require.Error(t, err)
}

// TestRevertRefusesAPathThatIsNotThere covers the revert of a path that
// matches neither a changed nor a tracked file.
func TestRevertRefusesAPathThatIsNotThere(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	err := ws.Revert(context.Background(), []string{"missing.txt"})
	require.ErrorContains(t, err, "does not match any file")

	err = ws.Revert(context.Background(), []string{"../escape"})
	require.Error(t, err)
}

// TestAbortRefusesWhenThereIsNothingToAbort covers the abort without a
// pending commit.
func TestAbortRefusesWhenThereIsNothingToAbort(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	err := ws.Abort(context.Background())
	require.ErrorContains(t, err, "nothing to abort")
}

// TestUpdateRefusesAWorkspaceThatIsNotOnABranch covers the update of a
// workspace that sits on a commit.
func TestUpdateRefusesAWorkspaceThatIsNotOnABranch(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)
	ws.state.Branch = ""
	require.NoError(t, ws.state.save(ws.Root()))

	_, err := ws.Update(context.Background())
	require.ErrorContains(t, err, "not on a branch")
}

// TestSparseRefusesAPrefixItCannotUse covers the sparse set with a prefix
// that is not a path of the repository.
func TestSparseRefusesAPrefixItCannotUse(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a/x.txt"}, []string{"one"})
	ws := cloneTestWorkspace(t, b)

	err := ws.Sparse(context.Background(), []string{"../escape"})
	require.ErrorContains(t, err, "sparse prefix")
}
