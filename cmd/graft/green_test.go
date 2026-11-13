package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestGreenSetAndShow drives the green command against a real server: the
// checkpoint of a branch is published and read back.
func TestGreenSetAndShow(t *testing.T) {
	env := startServer(t)
	tip := env.seedMain(t, map[string]string{"top.txt": "top\n"})

	target := []string{"-address", env.addr, "-storage", env.storage, "--repo", env.relPath}

	// The branch has no checkpoint.
	code, stdout, stderr := runCLI(t, append([]string{"green", "show", "--branch", "main"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "has no checkpoint")

	// The checkpoint is moved to the tip of the branch.
	code, stdout, stderr = runCLI(t, append([]string{"green", "set", "--branch", "main", "--commit", tip.String()}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, tip.String())

	// The branch is green: the checkpoint covers its tip.
	code, stdout, stderr = runCLI(t, append([]string{"green", "show", "--branch", "main", "--json"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	var res greenResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.Equal(t, tip.String(), res.Checkpoint)
	require.Equal(t, tip.String(), res.Tip)
	require.True(t, res.Green)
	require.False(t, res.Moved)

	// The branch moved on: the checkpoint is behind its tip, and the branch
	// is not green any more.
	second := env.seedMain(t, map[string]string{"top.txt": "top2\n"})
	code, stdout, stderr = runCLI(t, append([]string{"green", "show", "--branch", "main", "--json"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.Equal(t, tip.String(), res.Checkpoint)
	require.Equal(t, second.String(), res.Tip)
	require.False(t, res.Green)

	// The CI publishes the new checkpoint: the branch is green again.
	code, stdout, stderr = runCLI(t, append([]string{"green", "set", "--branch", "main", "--commit", second.String()}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "moved from "+tip.String())

	code, stdout, stderr = runCLI(t, append([]string{"green", "show", "--branch", "main", "--json"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.Equal(t, second.String(), res.Checkpoint)
	require.True(t, res.Green)
}

// TestGreenSetRejectsACommitThatDoesNotExist checks that the command does not
// move the checkpoint to a commit the repository does not hold.
func TestGreenSetRejectsACommitThatDoesNotExist(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})

	code, _, stderr := runCLI(t, "green", "set", "--branch", "main", "--commit", missingOID,
		"-address", env.addr, "-storage", env.storage, "--repo", env.relPath)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, missingOID)
}

// TestGreenUsage checks the command line of the green command.
func TestGreenUsage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{
			desc: "without a subcommand",
			args: []string{"green"},
			want: "green requires a subcommand",
		},
		{
			desc: "with an unknown subcommand",
			args: []string{"green", "move"},
			want: "unknown green subcommand",
		},
		{
			desc: "without a branch",
			args: []string{"green", "set", "--commit", "abc"},
			want: "requires --branch",
		},
		{
			desc: "without a commit",
			args: []string{"green", "set", "--branch", "main"},
			want: "requires --commit",
		},
		{
			desc: "without a branch to show",
			args: []string{"green", "show"},
			want: "requires --branch",
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

// TestGreenIsReadByTheGate checks that the checkpoint the command publishes is
// the one the branch management policy asks about.
func TestGreenIsReadByTheGate(t *testing.T) {
	env := startServer(t, func(c *config.Cfg) {
		c.Graft.Branches = []config.GraftBranchRule{{Name: "main", RequireVerified: true}}
	})
	tip := env.seedMain(t, map[string]string{"top.txt": "top\n"})

	target := []string{"-address", env.addr, "-storage", env.storage, "--repo", env.relPath}

	// The branch has no checkpoint: the policy refuses a change on it.
	code, _, stderr := runCLI(t, append([]string{"gate", "check", "--branch", "main"}, target...)...)
	require.Equal(t, exitError, code, stderr)

	// The CI publishes the checkpoint: the policy allows the change.
	code, _, stderr = runCLI(t, append([]string{"green", "set", "--branch", "main", "--commit", tip.String()}, target...)...)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, append([]string{"gate", "check", "--branch", "main", "--json"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	var res gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.True(t, res.Allowed)
	require.Equal(t, "allowed", res.Code)
	require.Equal(t, tip.String(), res.Checkpoint)
}
