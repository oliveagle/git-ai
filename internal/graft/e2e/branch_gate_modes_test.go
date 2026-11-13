package e2e

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/sdk"
)

// TestBranchGateAnswersInEveryStorageMode checks that the branch policy decides
// the same way whichever store holds the reviews: the files of the repository
// with a shadow copy of the storage core, and the database the core is
// authoritative over. The gate reads its state through one source
// (hook.BranchGateSource), and a mode it misread would answer a CI with a
// policy the server does not enforce.
func TestBranchGateAnswersInEveryStorageMode(t *testing.T) {
	forEachStorageDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		for _, mode := range []corestore.Mode{corestore.ModeShadow, corestore.ModeAuthoritative} {
			t.Run(string(mode), func(t *testing.T) {
				t.Parallel()

				requireBranchGateHoldsInMode(t, dialect, mode)
			})
		}
	})
}

// requireBranchGateHoldsInMode checks the decision and the enforcement of a
// branch rule on one node whose storage core runs in mode, with the loop a merge
// queue runs: real git and real reviews on one side, a named result and a
// published checkpoint on the other, in every mode.
func requireBranchGateHoldsInMode(t *testing.T, dialect sqlstore.Dialect, mode corestore.Mode) {
	t.Helper()

	ex := loadExample(t)
	ex.graft.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
	ex.graft.Policy = []config.GraftPolicyRule{
		{Refs: []string{"refs/heads/**"}, Allow: true},
		{Refs: []string{"refs/keep-around/**"}, Allow: true},
	}
	ex.graft.Branches = []config.GraftBranchRule{{Name: "main", RequireReview: true, RequireVerified: true}}

	cluster := newStorageCluster(t, dialect, mode, ex.graft)
	node := cluster.addNode(t)
	relPath := node.createRepository(t).GetRelativePath()

	builder := node.agent(t, relPath, exampleTokens["builder"])
	planner := node.agent(t, relPath, exampleTokens["planner"])
	operator := node.operator(t, relPath)

	// The server creates the first branch of a repository with the operator
	// credential: a client may not create a managed branch.
	base, err := commitFile(node.ctx, operator, "operator", "main", "", "README.md", "base\n")
	require.NoError(t, err)
	commit, err := commitFile(node.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	// A change without a review: the answer refuses it, and the land meets the
	// same refusal.
	answer, err := builder.BranchDecision(node.ctx, sdk.BranchDecisionOptions{Branch: "main", Commits: []string{commit}})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed())
	require.Equal(t, sdk.BranchNoReview, answer.GetCode())

	_, err = builder.Land(node.ctx, sdk.LandOptions{
		Branch: "main",
		Commit: commit,
		User:   sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no review")

	// The review is written where the mode keeps reviews, and the gate reads it
	// there: the answer turns into an allow, and the land lands.
	review, err := builder.CreateReview(node.ctx, sdk.CreateReviewOptions{
		TargetBranch: "main",
		Head:         commit,
		Base:         base,
		Title:        "land the change",
	})
	require.NoError(t, err)
	_, err = planner.ApproveReview(node.ctx, review.GetId(), "looks good")
	require.NoError(t, err)

	// The review is approved, and the change is still refused: the verification
	// checkpoint of the branch covers neither its tip nor the result.
	answer, err = builder.BranchDecision(node.ctx, sdk.BranchDecisionOptions{Branch: "main", Commits: []string{commit}})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed(), "mode %s: %s", mode, answer.GetReason())
	require.Equal(t, sdk.BranchNotVerified, answer.GetCode())
	require.Equal(t, 1, int(answer.GetReviews()))

	// The server names the commit the land would produce, and moves nothing.
	timestamp := time.Unix(1767225600, 0).UTC()
	land := sdk.LandOptions{
		Branch: "main",
		Commit: commit,
		User:   sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
		// The date is part of the commit's identity: the same date in the land
		// makes it write the very commit the dry run named.
		Timestamp: timestamp,
	}
	produced, err := builder.Land(node.ctx, sdk.LandOptions{
		Branch:    land.Branch,
		Commit:    land.Commit,
		User:      land.User,
		Timestamp: timestamp,
		DryRun:    true,
	})
	require.NoError(t, err)
	require.True(t, produced.DryRun)
	require.NotEmpty(t, produced.LandedCommitID)
	require.Equal(t, base, node.git(t, relPath, "rev-parse", "refs/heads/main"))

	// The result is named, and still refused while nothing verified it.
	answer, err = builder.BranchDecision(node.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed(), "mode %s: %s", mode, answer.GetReason())
	require.Equal(t, sdk.BranchNotVerified, answer.GetCode())
	require.Empty(t, answer.GetCheckpoint())

	// The CI verified that commit and the operator published it: the answer with
	// the result allows the change, and the land writes exactly the commit.
	require.NoError(t, operator.UpdateReference(node.ctx, "refs/graft/green/main", "", produced.LandedCommitID))

	answer, err = builder.BranchDecision(node.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "mode %s, reason: %s", mode, answer.GetReason())
	require.Equal(t, produced.LandedCommitID, answer.GetCheckpoint())

	landed, err := builder.Land(node.ctx, land)
	require.NoError(t, err)
	require.Equal(t, produced.LandedCommitID, landed.LandedCommitID)
	require.Equal(t, landed.LandedCommitID, node.git(t, relPath, "rev-parse", "refs/heads/main"))

	// The checkpoint covers the new tip: the branch is green the moment the land
	// is written, and the next change waits for its own verification.
	answer, err = builder.BranchDecision(node.ctx, sdk.BranchDecisionOptions{Branch: "main"})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "mode %s, reason: %s", mode, answer.GetReason())
	require.Equal(t, landed.LandedCommitID, answer.GetTip())
	require.Equal(t, landed.LandedCommitID, answer.GetCheckpoint())
}
