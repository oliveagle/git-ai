package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestGateCheck drives the gate command against a real server: the answer it
// prints is the one the server decides with.
func TestGateCheck(t *testing.T) {
	env := startServer(t, func(c *config.Cfg) {
		c.Graft.Branches = []config.GraftBranchRule{{Name: "main", RequireReview: true}}
	})
	env.seedMain(t, map[string]string{"top.txt": "top\n"})

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)

	// A change without a review does not land.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "doc.md"), []byte("docs\n"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "add docs")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "gate", "check", "-C", dir, "--branch", "main")
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stdout, "may not land on main")
	require.Contains(t, stdout, "has no review on the branch")

	// The pending commit is checked when the command names none.
	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "main", "--json")
	require.Equal(t, exitError, code, stderr)
	var res gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.True(t, res.Managed)
	require.False(t, res.Allowed)
	require.Equal(t, "no_review", res.Code)
	require.NotEmpty(t, res.Commit)
	require.Equal(t, "main", res.Branch)
	require.NotNil(t, res.Rule, "the rule that decided is printed with the answer")
	require.True(t, res.Rule.RequireReview)

	// A review of the change lets it land: a documentation change needs no
	// approval.
	code, _, stderr = runCLI(t, "review", "create", "-C", dir, "--branch", "main", "--title", "add docs", "--json")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "main", "--json")
	require.Equal(t, exitOK, code, stderr)
	var allowed gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &allowed))
	require.True(t, allowed.Allowed)
	require.Equal(t, "allowed", allowed.Code)
	require.Equal(t, 1, allowed.Reviews)

	// A branch without a rule is not managed.
	code, stdout, stderr = runCLI(t, "gate", "check", "-C", dir, "--branch", "other", "--json")
	require.Equal(t, exitOK, code, stderr)
	var unmanaged gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &unmanaged))
	require.False(t, unmanaged.Managed)
	require.True(t, unmanaged.Allowed)
	require.Equal(t, "not_managed", unmanaged.Code)
	require.Nil(t, unmanaged.Rule)
}

// TestGateCheckDirectUpdate asks the question the policy answers about a push:
// a branch that only a land may update refuses one, and asks the same question
// of an agent and a CI.
func TestGateCheckDirectUpdate(t *testing.T) {
	env := startServer(t, func(c *config.Cfg) {
		c.Graft.Branches = []config.GraftBranchRule{{Name: "main"}}
	})
	tip := env.seedMain(t, map[string]string{"top.txt": "top\n"})

	target := []string{"-address", env.addr, "-storage", env.storage, "--repo", env.relPath}

	code, stdout, stderr := runCLI(t, append([]string{"gate", "check", "--branch", "main", "--direct", "--json"}, target...)...)
	require.Equal(t, exitError, code, stderr)
	var res gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.False(t, res.Allowed)
	require.Equal(t, "direct_update_denied", res.Code)
	require.True(t, res.Direct)
	require.Equal(t, tip.String(), res.Tip)

	code, stdout, stderr = runCLI(t, append([]string{"gate", "check", "--branch", "main", "--direct"}, target...)...)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stdout, "may not update main")
}

// TestGateRules prints the policy the server enforces, so that the rules the
// check is made with are not a secret of the server configuration.
func TestGateRules(t *testing.T) {
	env := startServer(t, func(c *config.Cfg) {
		c.Graft.Branches = []config.GraftBranchRule{
			{Name: "main", RequireReview: true, RequireApprovals: 2, RequireVerified: true},
			{Name: "release/*", AllowDirectUpdate: true},
		}
	})

	target := []string{"-address", env.addr, "-storage", env.storage, "--repo", env.relPath}

	code, stdout, stderr := runCLI(t, append([]string{"gate", "rules", "--json"}, target...)...)
	require.Equal(t, exitOK, code, stderr)

	var res gateRulesResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.Len(t, res.Rules, 2)
	require.Equal(t, gateRule{
		Name:             "main",
		RequireReview:    true,
		RequireApprovals: 2,
		RequireVerified:  true,
	}, res.Rules[0])
	require.Equal(t, gateRule{Name: "release/*", AllowDirectUpdate: true}, res.Rules[1])

	code, stdout, stderr = runCLI(t, append([]string{"gate", "rules"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "main: only a land may update it; a review is required; 2 approval(s) are required; the verification checkpoint must cover the tip")
	require.Contains(t, stdout, "release/*: a push may update it")

	// A server that manages no branch says so.
	plain := startServer(t, func(*config.Cfg) {})
	code, stdout, stderr = runCLI(t, "gate", "rules", "-address", plain.addr, "-storage", plain.storage, "--repo", plain.relPath)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "the server manages no branch")
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
			desc: "with extra arguments",
			args: []string{"gate", "check", "extra"},
			want: "usage:",
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
