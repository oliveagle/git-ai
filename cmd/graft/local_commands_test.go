package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// TestRevertReportsWhatItReverted covers the two shapes of revert: every local
// change, and the paths the operator names. A path the workspace does not hold
// is refused.
func TestRevertReportsWhatItReverted(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a/x.txt", []byte("one"))
	useMemoryBackend(t, mem)
	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("two"), 0o644))
	code, stdout, stderr := runCLI(t, "revert", "-C", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "reverted all local changes")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("three"), 0o644))
	code, stdout, stderr = runCLI(t, "revert", "-C", dir, "a/x.txt")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "reverted a/x.txt")

	code, _, stderr = runCLI(t, "revert", "-C", dir, "nope.txt")
	require.Equal(t, exitError, code, stderr)
	require.NotEmpty(t, stderr)
}

// TestSparseReportsTheWholeRepository covers a workspace asked for no sparse
// path: the command says the whole repository is materialized.
func TestSparseReportsTheWholeRepository(t *testing.T) {
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a/x.txt", []byte("one"))
	useMemoryBackend(t, mem)
	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "sparse", "-C", dir)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "sparse: whole repository")
}
