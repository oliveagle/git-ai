package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// errBackend is the failure the failing backend reports. Every call of the
// backend fails with it, so the commands that talk to a server reach the
// branch that reports the failure instead of the one that works.
var errBackend = errors.New("the server refused the call")

// failingBackend is a backend whose every call fails. It stands for a server
// that is unreachable or refuses the request, and it lets the tests reach the
// error paths of the commands without a server that fails on demand.
type failingBackend struct {
	err error
}

func (b failingBackend) ResolveRevision(context.Context, string) (string, error) {
	return "", b.err
}

func (b failingBackend) ListTree(context.Context, string, string, bool) ([]workspace.Entry, error) {
	return nil, b.err
}

func (b failingBackend) GetObjects(context.Context, []string) (map[string][]byte, error) {
	return nil, b.err
}

func (b failingBackend) ReadFile(context.Context, string, string) ([]byte, error) {
	return nil, b.err
}

func (b failingBackend) CommitFiles(context.Context, workspace.CommitOptions) (string, error) {
	return "", b.err
}

func (b failingBackend) Land(context.Context, workspace.LandOptions) (workspace.LandResult, error) {
	return workspace.LandResult{}, b.err
}

func (b failingBackend) DeleteBranch(context.Context, string) error {
	return b.err
}

func (b failingBackend) Changesets(context.Context, uint64, int) ([]workspace.Changeset, error) {
	return nil, b.err
}

func (b failingBackend) WatchChangesets(context.Context, uint64) (<-chan workspace.Changeset, <-chan error) {
	changesets := make(chan workspace.Changeset)
	errs := make(chan error, 1)
	errs <- b.err
	close(errs)
	close(changesets)
	return changesets, errs
}

func (b failingBackend) Blame(context.Context, string, string) ([]workspace.BlameLine, error) {
	return nil, b.err
}

func (b failingBackend) PathHistory(context.Context, string, string, int) ([]workspace.Commit, error) {
	return nil, b.err
}

// useFailingBackend routes every command to a backend whose calls fail.
func useFailingBackend(t *testing.T) {
	t.Helper()
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) {
		return failingBackend{err: errBackend}, nil
	}
	t.Cleanup(func() { backendFactory = previous })
}

// clonedWorkspace clones a repository with the memory backend and returns the
// directory of the workspace.
func clonedWorkspace(t *testing.T, dir string) string {
	t.Helper()

	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a/x.txt", []byte("one"))
	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) { return mem, nil }
	t.Cleanup(func() { backendFactory = previous })

	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitOK, code, stderr)
	return dir
}

// TestCommandsReportTheFailureOfTheServer covers the paths of the commands
// that report the failure of the server instead of its answer.
func TestCommandsReportTheFailureOfTheServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	clonedWorkspace(t, dir)
	useFailingBackend(t)

	for _, args := range [][]string{
		{"update", "-C", dir},
		{"sparse", "-C", dir},
		{"log", "-C", dir},
		{"blame", "-C", dir, "a/x.txt"},
		{"history", "-C", dir, "a/x.txt"},
		{"cat", "-C", dir, "main:a/x.txt"},
	} {
		code, _, stderr := runCLI(t, args...)
		require.Equal(t, exitError, code, args)
		require.Contains(t, stderr, "the server refused the call", args)
	}
}

// TestWatchReportsTheFailureOfTheServer covers the watch, which reports the
// failure of the stream rather than returning it.
func TestWatchReportsTheFailureOfTheServer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	clonedWorkspace(t, dir)
	useFailingBackend(t)

	code, _, stderr := runCLI(t, "watch", "-C", dir)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "the server refused the call")
}

// TestCloneReportsTheFailureOfTheServer covers the clone, which never opens a
// workspace and reports the failure of the server directly.
func TestCloneReportsTheFailureOfTheServer(t *testing.T) {
	useFailingBackend(t)

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "memory", "repo", dir)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "the server refused the call")
}

// TestTheCommandsReportTheFailureOfTheBackendFactory covers the commands that
// report the failure of the factory itself, before any call reaches a server.
func TestTheCommandsReportTheFailureOfTheBackendFactory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	clonedWorkspace(t, dir)

	previous := backendFactory
	backendFactory = func(context.Context, target) (workspace.Backend, error) {
		return nil, errBackend
	}
	t.Cleanup(func() { backendFactory = previous })

	for _, args := range [][]string{
		{"update", "-C", dir},
		{"sparse", "-C", dir},
		{"log", "-C", dir},
		{"blame", "-C", dir, "a/x.txt"},
		{"history", "-C", dir, "a/x.txt"},
		{"cat", "-C", dir, "main:a/x.txt"},
	} {
		code, _, stderr := runCLI(t, args...)
		require.Equal(t, exitError, code, args)
		require.Contains(t, stderr, "the server refused the call", args)
	}
}

// TestCommitReportsTheFailureOfTheBackend covers the commit, which writes the
// files before it builds the commit.
func TestCommitReportsTheFailureOfTheBackend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	clonedWorkspace(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("changed"), 0o644))
	useFailingBackend(t)

	code, _, stderr := runCLI(t, "commit", "-C", dir, "-m", "change x")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "the server refused the call")
}

// TestLandReportsTheFailureOfTheBackend covers the land, which reports the
// failure of the server when it lands the pending commit.
func TestLandReportsTheFailureOfTheBackend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ws")
	clonedWorkspace(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "x.txt"), []byte("changed"), 0o644))

	code, _, stderr := runCLI(t, "commit", "-C", dir, "-m", "change x")
	require.Equal(t, exitOK, code, stderr)

	useFailingBackend(t)
	code, _, stderr = runCLI(t, "land", "-C", dir)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "the server refused the call")
}
