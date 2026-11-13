package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// gateConfigFile writes the configuration of a server that manages the branch
// main, and returns its path.
func gateConfigFile(t *testing.T, cfg config.Cfg) string {
	t.Helper()

	return testcfg.WriteTemporaryGitalyConfigFile(t, cfg)
}

// TestGateCheck drives the gate command against a real server.
func TestGateCheck(t *testing.T) {
	env := startServer(t, func(c *config.Cfg) {
		c.Graft.Branches = []config.GraftBranchRule{{Name: "main", RequireReview: true}}
	})
	env.seedMain(t, map[string]string{"top.txt": "top\n"})

	configPath := gateConfigFile(t, env.cfg)

	dir := filepath.Join(t.TempDir(), "ws")
	code, stdout, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	// A change without a review does not land.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("docs\n"), 0o644))
	code, stdout, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "add docs")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "main", "--config", configPath)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stdout, "may not land on main")
	require.Contains(t, stdout, "has no review on the branch")

	// The pending commit is checked when the command names none.
	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "main", "--config", configPath, "--json")
	require.Equal(t, exitError, code, stderr)
	var res gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.True(t, res.Managed)
	require.False(t, res.Allowed)
	require.Equal(t, "no_review", res.Code)
	require.NotEmpty(t, res.Commit)

	// A review of the change lets it land: a documentation change needs no
	// approval.
	code, stdout, stderr = runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "add docs", "--json")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "main", "--config", configPath, "--json")
	require.Equal(t, exitOK, code, stderr)
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.True(t, res.Allowed)
	require.Equal(t, "allowed", res.Code)
	require.Equal(t, 1, res.Reviews)

	// A branch without a rule is not managed.
	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "other", "--config", configPath, "--json")
	require.Equal(t, exitOK, code, stderr)
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.False(t, res.Managed)
	require.True(t, res.Allowed)
	require.Equal(t, "not_managed", res.Code)
}

// TestGateCheckUsage checks the command line of the gate command.
func TestGateCheckUsage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{
			desc: "without a subcommand",
			args: []string{"gate"},
			want: "gate requires a subcommand",
		},
		{
			desc: "with an unknown subcommand",
			args: []string{"gate", "test"},
			want: "unknown gate subcommand",
		},
		{
			desc: "without a branch",
			args: []string{"gate", "check"},
			want: "requires --branch",
		},
		{
			desc: "without a configuration file",
			args: []string{"gate", "check", "--branch", "main"},
			want: "requires --config",
		},
		{
			desc: "with a configuration file that the server would reject",
			args: []string{"gate", "check", "--branch", "main", "--config", "/does/not/exist.toml"},
			want: "/does/not/exist.toml",
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
