package workspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDryRunLandDefaultsToTheWorkspaceBranch checks the two defaults of a
// dry-run land: the branch of the workspace and the rebase strategy.
func TestDryRunLandDefaultsToTheWorkspaceBranch(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a.txt", "changed")

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change a", Author: Signature{Name: "dev"}})
	require.NoError(t, err)

	res, err := ws.DryRunLand(context.Background(), "", "", Signature{Name: "dev"}, time.Now())
	require.NoError(t, err)
	require.NotEmpty(t, res.Commit)

	// A dry run changes nothing.
	require.Equal(t, "a", readRemote(t, b, "main", "a.txt"))
}

// TestDryRunLandNeedsABranchOnADetachedWorkspace checks that a workspace at a
// commit has to be told where to land.
func TestDryRunLandNeedsABranchOnADetachedWorkspace(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	tip, err := b.ResolveRevision(context.Background(), "main")
	require.NoError(t, err)

	root := filepath.Join(t.TempDir(), "ws")
	ws, err := Clone(context.Background(), root, b, CloneOptions{At: tip})
	require.NoError(t, err)

	writeWorkspaceFile(t, root, "b.txt", "b")
	_, err = ws.Commit(context.Background(), CommitRequest{Message: "add b", Author: Signature{Name: "dev"}})
	require.NoError(t, err)

	_, err = ws.DryRunLand(context.Background(), "", StrategyRebase, Signature{Name: "dev"}, time.Now())
	require.ErrorContains(t, err, "name the branch to land on")
}

// TestStatusReportsATrackedFileThatBecameASymbolicLink checks that a symbolic
// link standing where a tracked file was is reported as a change and refused by
// the commit, instead of being followed or silently ignored.
func TestStatusReportsATrackedFileThatBecameASymbolicLink(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs a privilege on Windows")
	}

	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	ws := cloneTestWorkspace(t, b)

	full := filepath.Join(ws.Root(), "a.txt")
	require.NoError(t, os.Remove(full))
	require.NoError(t, os.Symlink(filepath.Join(ws.Root(), "elsewhere.txt"), full))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Equal(t, []Change{{Path: "a.txt", Kind: ChangeModified}}, changes)

	_, err = ws.Commit(context.Background(), CommitRequest{Message: "link", Author: Signature{Name: "dev"}})
	require.ErrorContains(t, err, "symbolic link")
	require.Nil(t, ws.Summary().Pending)
}

// TestStatusIgnoresSpecialFiles checks that a named pipe in the workspace is
// neither a change nor an error: reading it would block.
func TestStatusIgnoresSpecialFiles(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("named pipes are not created with mkfifo on Windows")
	}

	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	ws := cloneTestWorkspace(t, b)

	require.NoError(t, syscall.Mkfifo(filepath.Join(ws.Root(), "pipe"), 0o644))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
}

// TestCloneReportsARootItCannotRead checks a clone path under a file: the read
// of the directory fails with something other than "does not exist", which is
// reported instead of being taken for an absent directory.
func TestCloneReportsARootItCannotRead(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	file := filepath.Join(base, "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	_, err := Clone(context.Background(), filepath.Join(file, "ws"), b, CloneOptions{Branch: "main"})
	require.Error(t, err)
}

// TestCommitListsTheKeptFilesItDoesNotShow checks the message of a local file
// that stands where the server has a directory of files outside the sparse set:
// at most five are named, the rest are counted.
func TestCommitListsTheKeptFilesItDoesNotShow(t *testing.T) {
	t.Parallel()

	paths := []string{"d/1.txt", "d/2.txt", "d/3.txt", "d/4.txt", "d/5.txt", "d/6.txt", "other.txt"}
	contents := make([]string, len(paths))
	for i := range paths {
		contents[i] = paths[i]
	}
	b := seedBackend(t, paths, contents)
	ws := cloneTestWorkspace(t, b, "other")

	// The sparse set left d/ on the server; a local file named d is a conflict.
	writeWorkspaceFile(t, ws.Root(), "d", "local")

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "file for a directory", Author: Signature{Name: "dev"}})
	require.ErrorContains(t, err, "and 1 more")
	require.ErrorContains(t, err, ErrAlreadyExists.Error())
}
