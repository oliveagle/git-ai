package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestLandExpect drives the guarantee a merge queue needs: the land writes
// exactly the commit the CI verified, and refuses when the branch moved and
// the land would write another commit.
func TestLandExpect(t *testing.T) {
	env := startServer(t, func(c *config.Cfg) {
		c.Graft.Branches = []config.GraftBranchRule{{Name: "main", RequireVerified: true}}
	})
	base := env.seedMain(t, map[string]string{"top.txt": "top\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "change.txt"), []byte("change\n"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "add change")
	require.Equal(t, exitOK, code, stderr)

	timestamp := "2026-11-14T00:00:00Z"

	// The commit the CI verifies: the dry run names it.
	code, stdout, stderr := runCLI(t, "land", "-C", dir, "--dry-run", "--timestamp", timestamp)
	require.Equal(t, exitOK, code, stderr)
	verified := strings.TrimSpace(stdout)
	require.Len(t, verified, 40)

	// The date is part of the identity of the commit the land writes, so the
	// land cannot write the verified commit without it.
	code, _, stderr = runCLI(t, "land", "-C", dir, "--expect", verified)
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "--timestamp")

	// A dry run computes the commit; it cannot expect one.
	code, _, stderr = runCLI(t, "land", "-C", dir, "--dry-run", "--timestamp", timestamp, "--expect", verified)
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "--dry-run")

	// The branch moves while the suite runs: the land would write another
	// commit than the one the CI verified.
	tip := gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithBranch("main"),
		gittest.WithParents(base),
		gittest.WithTree(env.writeTree(t, map[string]string{"top.txt": "top\n", "notes.txt": "notes\n"})),
	)

	code, _, stderr = runCLI(t, "land", "-C", dir, "--timestamp", timestamp, "--expect", verified)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stderr, "the land would write")
	require.Contains(t, stderr, verified, "the message names the commit that was verified")
	require.Equal(t, tip.String(), gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/heads/main").String(),
		"a refused land writes nothing")

	// The queue computes the result again on the tip the branch holds, and
	// lands that one: the branch takes the commit that was verified.
	code, stdout, stderr = runCLI(t, "land", "-C", dir, "--dry-run", "--timestamp", timestamp)
	require.Equal(t, exitOK, code, stderr)
	verified = strings.TrimSpace(stdout)

	code, stdout, stderr = runCLI(t, "land", "-C", dir, "--timestamp", timestamp, "--expect", verified)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed at")
	require.Equal(t, verified, gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/heads/main").String())
}
