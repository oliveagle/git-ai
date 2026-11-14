package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestGreenPublishesTheSuites checks that the publisher says which suites ran
// on the tree the checkpoint certifies: the rule of the branch may demand the
// suites a change needs, and the CLI reads them back.
func TestGreenPublishesTheSuites(t *testing.T) {
	env := startServer(t, func(c *config.Cfg) {
		c.Graft.Branches = []config.GraftBranchRule{{Name: "main", RequireSuites: []string{"unit", "integration"}}}
	})
	tip := env.seedMain(t, map[string]string{"top.txt": "top\n"})

	target := []string{"-address", env.addr, "-storage", env.storage, "--repo", env.relPath}

	// The checkpoint and the suites are published together.
	code, stdout, stderr := runCLI(t, append([]string{
		"green", "set", "--branch", "main", "--commit", tip.String(),
		"--suite", "unit", "--suite", "integration",
	}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, tip.String())

	// The suites are read back: each one names the commit it ran on.
	code, stdout, stderr = runCLI(t, append([]string{"green", "show", "--branch", "main", "--json"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	var res greenResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.Len(t, res.Suites, 2)
	require.Equal(t, "unit", res.Suites[0].Name)
	require.Equal(t, tip.String(), res.Suites[0].Commit)
	require.Equal(t, "integration", res.Suites[1].Name)
	require.Equal(t, tip.String(), res.Suites[1].Commit)

	// The gate reads them too: the rule names the suites, and the checkpoint
	// certifies them.
	code, stdout, stderr = runCLI(t, append([]string{"gate", "check", "--branch", "main", "--json"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	var answer gateResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &answer))
	require.NotNil(t, answer.Rule)
	require.Equal(t, []string{"unit", "integration"}, answer.Rule.RequireSuites)
	require.ElementsMatch(t, []string{"unit", "integration"}, answer.CheckpointSuites)

	// A suite name that is not a reference component is refused: it would
	// name a branch instead.
	code, _, stderr = runCLI(t, append([]string{
		"green", "set", "--branch", "main", "--commit", tip.String(), "--suite", "unit/one",
	}, target...)...)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "not a valid suite name")
}
