package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// brokenBackend is a workspace backend that answers every call with the same
// failure. It is the server that went away under a command: every read and
// every write has to report it instead of pretending the workspace is empty.
type brokenBackend struct {
	workspace.Backend
	err     error
	landErr error
	// resolve is the commit ResolveRevision answers when it is set, so that
	// a command that resolves before it lands reaches the land.
	resolve string
}

func (b *brokenBackend) ResolveRevision(context.Context, string) (string, error) {
	if b.resolve != "" {
		return b.resolve, nil
	}
	return "", b.err
}

func (b *brokenBackend) ListTree(context.Context, string, string, bool) ([]workspace.Entry, error) {
	return nil, b.err
}

func (b *brokenBackend) GetObjects(context.Context, []string) (map[string][]byte, error) {
	return nil, b.err
}

func (b *brokenBackend) ReadFile(context.Context, string, string) ([]byte, error) {
	return nil, b.err
}

func (b *brokenBackend) CommitFiles(context.Context, workspace.CommitOptions) (string, error) {
	return "", b.err
}

func (b *brokenBackend) Land(context.Context, workspace.LandOptions) (workspace.LandResult, error) {
	if b.landErr != nil {
		return workspace.LandResult{}, b.landErr
	}
	return workspace.LandResult{}, b.err
}

func (b *brokenBackend) DeleteBranch(context.Context, string) error { return b.err }

func (b *brokenBackend) Changesets(context.Context, uint64, int) ([]workspace.Changeset, error) {
	return nil, b.err
}

func (b *brokenBackend) WatchChangesets(context.Context, uint64) (<-chan workspace.Changeset, <-chan error) {
	changes := make(chan workspace.Changeset)
	failures := make(chan error, 1)
	failures <- b.err
	close(failures)
	close(changes)
	return changes, failures
}

func (b *brokenBackend) Blame(context.Context, string, string) ([]workspace.BlameLine, error) {
	return nil, b.err
}

func (b *brokenBackend) PathHistory(context.Context, string, string, int) ([]workspace.Commit, error) {
	return nil, b.err
}

// brokenWorkspace clones a workspace from a memory backend, then routes the
// commands to a backend that fails: the workspace on disk is intact, and the
// server behind it is not.
func brokenWorkspace(t *testing.T) (string, *brokenBackend) {
	t.Helper()

	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a/x.txt", []byte("one"))
	useMemoryBackend(t, mem)
	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)

	broken := &brokenBackend{Backend: mem, err: errors.New("the server went away")}
	breakCPUTs(t, broken)
	return dir, broken
}

// breakCPUTs routes the commands to b until the test ends.
func breakCPUTs(t *testing.T, b *brokenBackend) {
	t.Helper()

	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) { return b, nil }
	t.Cleanup(func() { backendFactory = previous })
}

// pendingWorkspace clones a workspace and leaves a commit pending in it, so
// that a land has something to land.
func pendingWorkspace(t *testing.T) (string, *workspace.MemoryBackend) {
	t.Helper()

	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a/x.txt", []byte("one"))
	useMemoryBackend(t, mem)
	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("changed"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-m", "change a/x.txt")
	require.Equal(t, exitOK, code, stderr)
	return dir, mem
}

// TestCommandsReportABackendThatFails covers the reads and the local writes of
// a workspace whose server fails: each command answers with the failure, and
// none of them prints a result that reads like a success.
func TestCommandsReportABackendThatFails(t *testing.T) {
	dir, _ := brokenWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("changed"), 0o644))

	for _, args := range [][]string{
		{"show", "-C", dir, "main"},
		{"cat", "-C", dir, "main:a/x.txt"},
		{"blame", "-C", dir, "a/x.txt"},
		{"history", "-C", dir, "a/x.txt"},
		{"log", "-C", dir},
		{"commit", "-C", dir, "-m", "change", "-land", "main"},
		{"land", "-C", dir, "main"},
		{"update", "-C", dir},
		{"sparse", "-C", dir, "b"},
		{"watch", "-C", dir},
	} {
		code, stdout, stderr := runCLI(t, args...)
		require.Equal(t, exitError, code, "%v: stdout=%q stderr=%q", args, stdout, stderr)
		require.NotEmpty(t, stderr, "%v", args)
	}
}

// TestLandPrintsTheConflictPackageOfAFailingBackend covers the conflict
// package a land carries: it is printed, and a conflicts directory that cannot
// be written is reported beside it.
func TestLandPrintsTheConflictPackageOfAFailingBackend(t *testing.T) {
	dir, mem := pendingWorkspace(t)
	broken := &brokenBackend{Backend: mem, err: errors.New("the server went away")}
	breakCPUTs(t, broken)
	st, err := status.New(codes.FailedPrecondition, "the land did not resolve every file").
		WithDetails(&gitalypb.LandConflict{
			Tip:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Commit: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Files: []*gitalypb.LandConflictFile{{
				Path:    []byte("a/x.txt"),
				Outcome: gitalypb.LandConflictFile_OUTCOME_FAILED,
				Reason:  "both sides changed the line",
			}},
			Messages: []*gitalypb.LandConflictMessage{{Message: "CONFLICT (content)"}},
		})
	require.NoError(t, err)
	broken.landErr = st.Err()

	code, stdout, stderr := runCLI(t, "land", "-C", dir, "main")
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stdout, "the land did not resolve 1 conflicting files")
	require.Contains(t, stdout, "a/x.txt: failed: both sides changed the line")
	require.Contains(t, stdout, "merge: CONFLICT (content)")

	// A conflicts directory that is a file cannot hold the package, and the
	// command says which failure the land and the write both were.
	blocked := filepath.Join(t.TempDir(), "conflicts")
	require.NoError(t, os.WriteFile(blocked, []byte("not a directory"), 0o644))
	code, stdout, stderr = runCLI(t, "land", "-C", dir, "main", "--resolve", "--conflicts-dir", blocked)
	require.Equal(t, exitError, code)
	require.Contains(t, stdout, "the land did not resolve 1 conflicting files")
	require.Contains(t, stderr, "writing the conflicts failed")

	// The land of a named commit resolves it first, then prints the package:
	// it has no workspace, so it never writes a directory.
	broken.resolve = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	code, stdout, stderr = runCLI(t, "land", "--commit", "bbbb", "main", "-address", "memory", "-repo", "repo")
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stdout, "the land did not resolve 1 conflicting files")
}
