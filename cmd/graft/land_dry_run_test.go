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

// TestLandDryRun drives the loop a merge queue runs from a script: the land
// names the commit it would produce, the checkpoint is published on that commit,
// and the land then produces it, so a branch that only takes verified lands is
// green when it is written.
func TestLandDryRun(t *testing.T) {
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

	// The branch moves on while the change is in review, so the land rewrites
	// the change and the commit it produces is not the commit that was committed.
	tip := gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithBranch("main"),
		gittest.WithParents(base),
		gittest.WithTree(env.writeTree(t, map[string]string{"top.txt": "top\n", "notes.txt": "notes\n"})),
	)
	timestamp := "2026-11-14T00:00:00Z"

	// A dry run prints the commit the land would produce, and nothing of the
	// branch changes.
	code, stdout, stderr := runCLI(t, "land", "-C", dir, "--dry-run", "--timestamp", timestamp)
	require.Equal(t, exitOK, code, stderr)
	produced := strings.TrimSpace(stdout)
	require.Len(t, produced, 40, "the output is the object ID alone, for a script")
	require.Equal(t, tip.String(), gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/heads/main").String())

	// The date is part of the identity of the commit the land writes: without
	// it, the dry run cannot name the commit a land would produce.
	code, _, stderr = runCLI(t, "land", "-C", dir, "--dry-run")
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "--timestamp")

	// The gate refuses the change until the checkpoint covers the commit the
	// land produces, and allows it once it does.
	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "main", "--result", produced, "--json")
	require.Equal(t, exitError, code, stderr)
	var refused gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &refused))
	require.Equal(t, "not_verified", refused.Code)

	gittest.Exec(t, env.cfg, "-C", env.repoPath, "update-ref", "refs/graft/green/main", produced)

	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "main", "--result", produced, "--json")
	require.Equal(t, exitOK, code, stderr)
	var allowed gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &allowed))
	require.True(t, allowed.Allowed, "reason: %s", allowed.Reason)

	// The land produces exactly the commit the CI verified.
	code, stdout, stderr = runCLI(t, "land", "-C", dir, "--timestamp", timestamp)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "landed at")
	require.Equal(t, produced, gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/heads/main").String())
	require.Equal(t, produced, gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/graft/green/main").String())
}
