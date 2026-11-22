package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/sdk"
)

// TestBranchGateRequiresTheSuites checks the rule of a project whose change
// needs more than one suite: the checkpoint has to certify every suite the
// rule names, so that the unit suite alone does not carry the land.
//
// The suites are published next to the checkpoint, one reference each, naming
// the commit the suite ran on.
func TestBranchGateRequiresTheSuites(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{
		Name:                  "main",
		RequireVerifiedResult: true,
		RequireSuites:         []string{"unit", "integration"},
	})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	land := sdk.LandOptions{
		Branch:    "main",
		Commit:    commit,
		User:      sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
		Timestamp: time.Unix(1700000000, 0),
	}

	// The CI asks what the land would produce, and verifies that commit.
	dry := land
	dry.DryRun = true
	produced, err := builder.Land(s.ctx, dry)
	require.NoError(t, err)
	require.NotEmpty(t, produced.LandedCommitID)

	// The checkpoint covers the result, but no suite ran on it: the rule of
	// the branch refuses the land, and says which suites are missing.
	s.git(t, "update-ref", "refs/graft/green/main", produced.LandedCommitID)

	_, err = builder.Land(s.ctx, land)
	require.Error(t, err)
	require.Equal(t, "suites_missing", sdk.BranchGateCode(err))

	// The unit suite ran on the result, and the integration suite did not:
	// the rule still refuses the land.
	s.git(t, "update-ref", "refs/graft/green-suite/unit/main", produced.LandedCommitID)

	_, err = builder.Land(s.ctx, land)
	require.Error(t, err)
	require.Equal(t, "suites_missing", sdk.BranchGateCode(err))

	// Both suites ran on the result: the branch takes the commit that was
	// tested.
	s.git(t, "update-ref", "refs/graft/green-suite/integration/main", produced.LandedCommitID)

	landed, err := builder.Land(s.ctx, land)
	require.NoError(t, err)
	require.Equal(t, produced.LandedCommitID, landed.LandedCommitID)
	require.Equal(t, produced.LandedCommitID, s.git(t, "rev-parse", "refs/heads/main"))
}
