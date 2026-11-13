package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestLandCommitWithoutAWorkspace drives the merge queue loop of a CI that
// holds a commit and a server, not a checkout: the dry run names the commit the
// land would produce, the checkpoint is published on it, and the land of the
// same commit produces exactly it.
func TestLandCommitWithoutAWorkspace(t *testing.T) {
	env := startServer(t, func(c *config.Cfg) {
		c.Graft.Branches = []config.GraftBranchRule{{Name: "main", RequireVerified: true}}
	})
	base := env.seedMain(t, map[string]string{"top.txt": "top\n"})

	// The change lives on the temporary branch of the agent that made it.
	change := gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithBranch("graft/ci/change"),
		gittest.WithParents(base),
		gittest.WithTree(env.writeTree(t, map[string]string{"top.txt": "top\n", "change.txt": "change\n"})),
		gittest.WithMessage("add change"),
	)

	target := []string{"-address", env.addr, "-storage", env.storage, "--repo", env.relPath}
	timestamp := "2026-11-14T00:00:00Z"

	// The dry run names the commit the land would produce and moves nothing.
	code, stdout, stderr := runCLI(t, append([]string{
		"land", "main", "--commit", change.String(), "--dry-run", "--timestamp", timestamp,
	}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	produced := strings.TrimSpace(stdout)
	require.Len(t, produced, 40, "the output is the object ID alone, for a script")
	require.Equal(t, base.String(), gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/heads/main").String())

	// The CI materializes the commit the dry run named: the tree it tests is the
	// tree the branch will hold, not the change on the tip it started from. The
	// workspace is at that commit, not on a branch, and says so.
	dir := filepath.Join(t.TempDir(), "verify")
	code, stdout, stderr = runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir, "--at", produced)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "(commit "+produced[:7])
	content, err := os.ReadFile(filepath.Join(dir, "change.txt"))
	require.NoError(t, err)
	require.Equal(t, "change\n", string(content))

	code, _, stderr = runCLI(t, "update", "-C", dir)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "not on a branch")

	// The gate refuses the change until the checkpoint covers the commit the
	// land produces.
	code, stdout, stderr = runCLI(t, append([]string{
		"gate", "check", "--branch", "main", "--commit", change.String(), "--result", produced, "--json",
	}, target...)...)
	require.Equal(t, exitError, code, stderr)
	var refused gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &refused))
	require.Equal(t, "not_verified", refused.Code)

	code, _, stderr = runCLI(t, append([]string{
		"green", "set", "--branch", "main", "--commit", produced,
	}, target...)...)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, append([]string{
		"gate", "check", "--branch", "main", "--commit", change.String(), "--result", produced, "--json",
	}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	var allowed gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &allowed))
	require.True(t, allowed.Allowed, "reason: %s", allowed.Reason)

	// The land produces exactly the commit the CI verified.
	code, stdout, stderr = runCLI(t, append([]string{
		"land", "main", "--commit", change.String(), "--timestamp", timestamp,
	}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed at")
	require.Equal(t, produced, gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/heads/main").String())
	require.Equal(t, produced, gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/graft/green/main").String())
}

// TestLandCommitRejectsACommitTheServerDoesNotHold checks that a mistyped object
// ID is named as such, before anything is landed.
func TestLandCommitRejectsACommitTheServerDoesNotHold(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})

	code, _, stderr := runCLI(t, "land", "main", "--commit", missingOID,
		"-address", env.addr, "-storage", env.storage, "--repo", env.relPath)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, missingOID)
}

// TestLandCommitUsage checks the command line of a land of a named commit.
func TestLandCommitUsage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{
			desc: "without a branch",
			args: []string{"land", "--commit", "abc"},
			want: "requires BRANCH",
		},
		{
			desc: "with a resolution",
			args: []string{"land", "main", "--commit", "abc", "--resolve"},
			want: "takes neither --abort, --resolve",
		},
		{
			desc: "with a conflicts directory",
			args: []string{"land", "main", "--commit", "abc", "--conflicts-dir", "conflicts"},
			want: "takes neither --abort, --resolve",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			code, _, stderr := runCLI(t, tc.args...)
			require.Equal(t, exitUsage, code)
			require.Contains(t, stderr, tc.want)
		})
	}
}
