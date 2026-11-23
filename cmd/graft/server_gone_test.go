package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// serverGone is the address of a server that refuses every connection: the
// commands reach their request and report the server, instead of hanging or
// printing a result.
const serverGone = "tcp://127.0.0.1:1"

// TestReviewCommandsReportAServerThatIsNotThere covers the review commands
// against a server that is gone: each one reports the failure of its request.
func TestReviewCommandsReportAServerThatIsNotThere(t *testing.T) {
	for _, args := range [][]string{
		{"review", "show", "1"},
		{"review", "list"},
		{"review", "comment", "1", "--body", "one"},
		{"review", "approve", "1"},
		{"review", "request-changes", "1", "--body", "no"},
		{"review", "update", "1", "--head", "0123456789012345678901234567890123456789"},
		{"review", "close", "1"},
		{"review", "create", "--branch", "main", "--title", "t", "--head", "0123456789012345678901234567890123456789"},
	} {
		t.Run(strings.Join(args[1:2], " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(t, append(args,
				"-address", serverGone, "-storage", "default", "-repo", "repo")...)
			require.Equal(t, exitError, code, "%v: stdout=%q stderr=%q", args, stdout, stderr)
			require.NotEmpty(t, stderr, "%v", args)
		})
	}
}

// TestReviewCreateNeedsAPendingCommit covers the head a review is opened on:
// a workspace with no pending commit cannot tell what to review, and the
// command says so before it asks the server anything.
func TestReviewCreateNeedsAPendingCommit(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"a/x.txt": "one\n"})
	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, "review", "create", "--branch", "main", "--title", "t", "-C", dir)
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "no pending commit to review")
}

// TestGreenCommandsReportAServerThatIsNotThere covers green against a server
// that is gone, and the workspace whose tree cannot be read.
func TestGreenCommandsReportAServerThatIsNotThere(t *testing.T) {
	for _, args := range [][]string{
		{"green", "show", "--branch", "main"},
		{"green", "set", "--branch", "main", "--commit", "0123456789012345678901234567890123456789"},
	} {
		t.Run(args[1], func(t *testing.T) {
			code, _, stderr := runCLI(t, append(args,
				"-address", serverGone, "-storage", "default", "-repo", "repo")...)
			require.Equal(t, exitError, code, stderr)
			require.NotEmpty(t, stderr)
		})
	}

	// The set in a workspace whose server is gone cannot read the tree it
	// would certify.
	dir, _ := brokenWorkspace(t)
	code, _, stderr := runCLI(t, "green", "set", "--branch", "main", "--commit", "0123456789012345678901234567890123456789", "-C", dir)
	require.Equal(t, exitError, code, stderr)

	// A workspace that is a file cannot be opened.
	notADir := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, os.WriteFile(notADir, []byte("not a workspace"), 0o644))
	code, _, stderr = runCLI(t, "green", "show", "--branch", "main", "-C", notADir)
	require.Equal(t, exitError, code, stderr)
}

// TestGatePolicyAndSearchReportAServerThatIsNotThere covers the commands that
// ask the server a question without a workspace.
func TestGatePolicyAndSearchReportAServerThatIsNotThere(t *testing.T) {
	for _, args := range [][]string{
		{"gate", "check", "--branch", "main", "--commit", "0123456789012345678901234567890123456789", "-repo", "repo"},
		{"search", "needle", "--rev", "main"},
	} {
		t.Run(args[0], func(t *testing.T) {
			code, stdout, stderr := runCLI(t, append(args,
				"-address", serverGone, "-storage", "default")...)
			require.Equal(t, exitError, code, "%v: stdout=%q stderr=%q", args, stdout, stderr)
			require.NotEmpty(t, stderr)
		})
	}

	// The listing of a storage names no repository.
	code, _, stderr := runCLI(t, "storage", "archived", "-address", serverGone, "-storage", "default")
	require.Equal(t, exitError, code, stderr)
	require.NotEmpty(t, stderr)
}

// TestStorageCommandsReportAServerThatIsNotThere covers the operator's storage
// commands against a server that is gone: the repository is opened remotely,
// so every one of them reports the connection instead of a result.
func TestStorageCommandsReportAServerThatIsNotThere(t *testing.T) {
	for _, args := range [][]string{
		{"storage", "state"},
		{"storage", "verify"},
		{"storage", "checkpoint"},
		{"storage", "gc"},
		{"storage", "archive"},
		{"storage", "unarchive"},
		{"storage", "delete-conflicting-ref", "refs/heads/a", "0123456789012345678901234567890123456789"},
	} {
		t.Run(args[1], func(t *testing.T) {
			code, stdout, stderr := runCLI(t, append(args,
				"-address", serverGone, "-storage", "default", "-repo", "repo")...)
			require.Equal(t, exitError, code, "%v: stdout=%q stderr=%q", args, stdout, stderr)
			require.NotEmpty(t, stderr)
		})
	}

	// The listing of a storage names no repository.
	code, _, stderr := runCLI(t, "storage", "archived", "-address", serverGone, "-storage", "default")
	require.Equal(t, exitError, code, stderr)
	require.NotEmpty(t, stderr)
}
