package e2e

import (
	"testing"

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
// branch rule on one node whose storage core runs in mode.
func requireBranchGateHoldsInMode(t *testing.T, dialect sqlstore.Dialect, mode corestore.Mode) {
	t.Helper()

	ex := loadExample(t)
	ex.graft.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
	ex.graft.Policy = []config.GraftPolicyRule{
		{Refs: []string{"refs/heads/**"}, Allow: true},
		{Refs: []string{"refs/keep-around/**"}, Allow: true},
	}
	ex.graft.Branches = []config.GraftBranchRule{{Name: "main", RequireReview: true}}

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

	answer, err = builder.BranchDecision(node.ctx, sdk.BranchDecisionOptions{Branch: "main", Commits: []string{commit}})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "mode %s, reason: %s", mode, answer.GetReason())
	require.Equal(t, 1, int(answer.GetReviews()))

	landed, err := builder.Land(node.ctx, sdk.LandOptions{
		Branch: "main",
		Commit: commit,
		User:   sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, landed.LandedCommitID)
	require.Equal(t, landed.LandedCommitID, node.git(t, relPath, "rev-parse", "refs/heads/main"))
}
