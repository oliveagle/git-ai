package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRepoCreateChecksItsFlagsBeforeTheServer covers the flags of repo create
// that are refused without a server: the object format and a branch without the
// initial commit.
func TestRepoCreateChecksItsFlagsBeforeTheServer(t *testing.T) {
	t.Parallel()

	code, _, stderr := runCLI(t, "repo", "create", "--object-format", "svn", "x.git")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "--object-format must be sha1 or sha256")

	code, _, stderr = runCLI(t, "repo", "create", "--branch", "next", "x.git")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "requires --initial-commit")
}
