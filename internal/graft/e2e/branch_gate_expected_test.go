package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/sdk"
)

// TestBranchGateLandsTheVerifiedCommit checks the guarantee a merge queue
// needs: the land writes exactly the commit the CI verified. The branch moves
// after the verification, and the land refuses instead of taking a commit
// nobody tested; the queue computes the result again and lands that one.
func TestBranchGateLandsTheVerifiedCommit(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireVerifiedResult: true})
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

	// The CI publishes it as the checkpoint of the branch: the operator writes
	// the checkpoint, an agent cannot.
	s.git(t, "update-ref", "refs/graft/green/main", produced.LandedCommitID)

	answer, err := builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "reason: %s", answer.GetReason())

	// The branch moves after the verification: the land would replay the change
	// onto the new tip and write another commit than the one the CI verified.
	tip := writeMain(t, s, map[string]string{"README.md": "main\n", "NOTES.md": "notes\n"}, base)

	verified := land
	verified.ExpectedResult = produced.LandedCommitID
	_, err = builder.Land(s.ctx, verified)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, sdk.Code(err))

	// The failure names the commit the land would have written, so that the
	// queue knows it has to compute the result again.
	detail, ok := sdk.ExpectedResultMismatch(err)
	require.True(t, ok)
	require.Equal(t, produced.LandedCommitID, detail.GetExpected())
	require.NotEqual(t, produced.LandedCommitID, detail.GetLanded())
	require.Equal(t, tip, detail.GetTip())

	// Nothing was written: the branch holds the commit it held before.
	require.Equal(t, tip, s.git(t, "rev-parse", "refs/heads/main"))

	// The queue computes the result again on the tip the branch holds, publishes
	// it as the checkpoint, and lands that one.
	dry = land
	dry.DryRun = true
	produced, err = builder.Land(s.ctx, dry)
	require.NoError(t, err)
	require.NotEqual(t, tip, produced.LandedCommitID)

	s.git(t, "update-ref", "refs/graft/green/main", produced.LandedCommitID)

	verified = land
	verified.ExpectedResult = produced.LandedCommitID
	landed, err := builder.Land(s.ctx, verified)
	require.NoError(t, err)
	require.Equal(t, produced.LandedCommitID, landed.LandedCommitID)
	require.Equal(t, produced.LandedCommitID, s.git(t, "rev-parse", "refs/heads/main"))
	require.Equal(t, produced.LandedCommitID, s.git(t, "rev-parse", "refs/graft/green/main"))
}

// TestBranchGateRefusesALandWithoutItsResult checks that a land that names no
// result is refused by the rule of a merge queue, and that the refusal names
// the code the queue reads.
func TestBranchGateRefusesALandWithoutItsResult(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireVerifiedResult: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	_, err = builder.Land(s.ctx, sdk.LandOptions{
		Branch:    "main",
		Commit:    commit,
		User:      sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
		Timestamp: time.Unix(1700000000, 0),
	})
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Contains(t, err.Error(), "no verification checkpoint")

	// The refusal carries the code of the decision, so that a queue branches
	// on the code instead of parsing the message.
	require.Equal(t, "not_verified", sdk.BranchGateCode(err))
}
