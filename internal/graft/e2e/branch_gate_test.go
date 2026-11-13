package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/sdk"
)

// startWithBranchRule starts the example server with a branch management
// policy whose rule for main is rule. The access policy lets every agent
// update every branch, so that the branch gate is what decides.
func startWithBranchRule(t *testing.T, rule config.GraftBranchRule) gitalyStack {
	t.Helper()

	ex := loadExample(t)
	ex.graft.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
	ex.graft.Policy = []config.GraftPolicyRule{
		{Refs: []string{"refs/heads/**"}, Allow: true},
		// The keep-around references of a review are written by the server
		// with the identity of the agent that opened it.
		{Refs: []string{"refs/keep-around/**"}, Allow: true},
	}
	ex.graft.Branches = []config.GraftBranchRule{rule}

	return startGitalyInMode(t, ex.graft, ex.sharedToken, true)
}

// startWithBranchGate starts the example server with a branch management
// policy that manages main: only a land with an approved review may update it.
func startWithBranchGate(t *testing.T) gitalyStack {
	t.Helper()

	return startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireReview: true})
}

// TestBranchGateHoldsOnAgentLands checks that the branch management policy
// decides the updates of an agent: a push to the managed branch is refused, a
// land without an approved review is refused, and a land with one is accepted.
func TestBranchGateHoldsOnAgentLands(t *testing.T) {
	t.Parallel()

	s := startWithBranchGate(t)
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	planner := s.repoAs(t, exampleTokens["planner"])

	// The builder works on a branch of its own.
	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	// A push to the managed branch is refused: only a land may update it.
	clientPath := filepath.Join(t.TempDir(), "client")
	gittest.Exec(t, s.cfg, "clone", "--quiet", s.repoPath, clientPath)
	require.NoError(t, os.WriteFile(filepath.Join(clientPath, "pushed.go"), []byte("package main\n"), 0o644))
	gittest.Exec(t, s.cfg, "-C", clientPath, "add", "pushed.go")
	gittest.Exec(t, s.cfg, "-C", clientPath, "-c", "user.name=builder", "-c", "user.email=builder@example.com", "commit", "--quiet", "-m", "pushed")
	pushed := strings.TrimSpace(string(gittest.Exec(t, s.cfg, "-C", clientPath, "rev-parse", "HEAD")))

	out, ok := agentPush(t, s, clientPath, exampleTokens["builder"], "builder", "HEAD:refs/heads/main")
	require.False(t, ok, out)
	require.Contains(t, out, "update aborted by the reference-transaction hook")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))
	require.NotEqual(t, pushed, s.git(t, "rev-parse", "refs/heads/main"))

	// A land without a review of the change is refused.
	_, err = landAs(s.ctx, builder, "builder", "main", commit)
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no review")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	// A review of the change, approved by another agent, lets it land.
	review, err := builder.CreateReview(s.ctx, sdk.CreateReviewOptions{
		TargetBranch: "main",
		Head:         commit,
		Base:         base,
		Title:        "land the change",
	})
	require.NoError(t, err)
	require.False(t, review.GetApproved(), "the review of a code change needs an approval")

	_, err = planner.ApproveReview(s.ctx, review.GetId(), "looks good")
	require.NoError(t, err)

	landed, err := landAs(s.ctx, builder, "builder", "main", commit)
	require.NoError(t, err)
	require.NotEmpty(t, landed.LandedCommitID)
	require.NotEqual(t, base, s.git(t, "rev-parse", "refs/heads/main"))
}

// TestBranchGateAllowsDirectUpdatesWhenTheRuleSays checks that a branch whose
// rule allows direct updates is pushed to as any other branch.
func TestBranchGateAllowsDirectUpdatesWhenTheRuleSays(t *testing.T) {
	t.Parallel()

	ex := loadExample(t)
	ex.graft.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
	ex.graft.Policy = []config.GraftPolicyRule{
		{Refs: []string{"refs/heads/**"}, Allow: true},
		{Refs: []string{"refs/keep-around/**"}, Allow: true},
	}
	ex.graft.Branches = []config.GraftBranchRule{{Name: "main", AllowDirectUpdate: true}}

	s := startGitalyInMode(t, ex.graft, ex.sharedToken, true)
	writeMain(t, s, map[string]string{"README.md": "main\n"})

	clientPath := filepath.Join(t.TempDir(), "client")
	gittest.Exec(t, s.cfg, "clone", "--quiet", s.repoPath, clientPath)
	require.NoError(t, os.WriteFile(filepath.Join(clientPath, "pushed.go"), []byte("package main\n"), 0o644))
	gittest.Exec(t, s.cfg, "-C", clientPath, "add", "pushed.go")
	gittest.Exec(t, s.cfg, "-C", clientPath, "-c", "user.name=builder", "-c", "user.email=builder@example.com", "commit", "--quiet", "-m", "pushed")
	pushed := strings.TrimSpace(string(gittest.Exec(t, s.cfg, "-C", clientPath, "rev-parse", "HEAD")))

	out, ok := agentPush(t, s, clientPath, exampleTokens["builder"], "builder", "HEAD:refs/heads/main")
	require.True(t, ok, out)
	require.Equal(t, pushed, s.git(t, "rev-parse", "refs/heads/main"))
}

// TestBranchGateRequiresAVerifiedCheckpoint checks that a branch whose rule
// asks for a verification checkpoint takes no land while the checkpoint does
// not cover the tip the land builds on: the branch is green only when
// graft-verify moved its checkpoint to that tip.
func TestBranchGateRequiresAVerifiedCheckpoint(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireVerified: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])

	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	// The branch has no checkpoint: the land is refused.
	_, err = landAs(s.ctx, builder, "builder", "main", commit)
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification checkpoint")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	// The verifier moved the checkpoint to the tip of the branch: the land
	// goes on.
	s.git(t, "update-ref", "refs/graft/green/main", base)

	landed, err := landAs(s.ctx, builder, "builder", "main", commit)
	require.NoError(t, err)
	require.NotEmpty(t, landed.LandedCommitID)
	tip := s.git(t, "rev-parse", "refs/heads/main")
	require.NotEqual(t, base, tip)

	// The checkpoint is behind the new tip: the branch is not green any more,
	// and the next land waits for the verifier.
	second, err := commitFile(s.ctx, builder, "builder", "feature2", tip, "other.go", "package main\n")
	require.NoError(t, err)

	_, err = landAs(s.ctx, builder, "builder", "main", second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification checkpoint")
	require.Equal(t, tip, s.git(t, "rev-parse", "refs/heads/main"))
}

// TestBranchGateRefusesAStaleReview checks that a review based on an older
// tip of the branch does not land: the review must cover what the branch
// holds.
func TestBranchGateRefusesAStaleReview(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireReview: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	planner := s.repoAs(t, exampleTokens["planner"])

	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	review, err := builder.CreateReview(s.ctx, sdk.CreateReviewOptions{
		TargetBranch: "main",
		Head:         commit,
		Base:         base,
		Title:        "land the change",
	})
	require.NoError(t, err)
	_, err = planner.ApproveReview(s.ctx, review.GetId(), "looks good")
	require.NoError(t, err)

	// The branch moved on: the review is based on an older tip.
	writeMain(t, s, map[string]string{"README.md": "main2\n"}, base)

	_, err = landAs(s.ctx, builder, "builder", "main", commit)
	require.Error(t, err)
	require.Contains(t, err.Error(), "is based on")
}

// TestBranchGateAllowsAStaleReviewWhenTheRuleSays checks that a rule that
// allows a stale base lets a change land with a review of an older tip.
func TestBranchGateAllowsAStaleReviewWhenTheRuleSays(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireReview: true, AllowStaleBase: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	planner := s.repoAs(t, exampleTokens["planner"])

	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	review, err := builder.CreateReview(s.ctx, sdk.CreateReviewOptions{
		TargetBranch: "main",
		Head:         commit,
		Base:         base,
		Title:        "land the change",
	})
	require.NoError(t, err)
	_, err = planner.ApproveReview(s.ctx, review.GetId(), "looks good")
	require.NoError(t, err)

	// The branch moved on: the review is based on an older tip, and the rule
	// allows it.
	writeMain(t, s, map[string]string{"README.md": "main2\n"}, base)

	landed, err := landAs(s.ctx, builder, "builder", "main", commit)
	require.NoError(t, err)
	require.NotEmpty(t, landed.LandedCommitID)
}

// TestBranchGateCountsTheApprovalsOfTheReview checks that a rule that asks
// for more approvals than the review rules do waits for the missing ones.
func TestBranchGateCountsTheApprovalsOfTheReview(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireReview: true, RequireApprovals: 2})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	planner := s.repoAs(t, exampleTokens["planner"])

	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	review, err := builder.CreateReview(s.ctx, sdk.CreateReviewOptions{
		TargetBranch: "main",
		Head:         commit,
		Base:         base,
		Title:        "land the change",
	})
	require.NoError(t, err)

	// One approval is not enough: the rule of the branch asks for two.
	_, err = planner.ApproveReview(s.ctx, review.GetId(), "looks good")
	require.NoError(t, err)

	_, err = landAs(s.ctx, builder, "builder", "main", commit)
	require.Error(t, err)
	require.Contains(t, err.Error(), "approvals")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))
}

// TestBranchGateAnswersWhatItEnforces checks the property a CI relies on: the
// answer the server gives before a land is the answer it gives when the land
// arrives, so that a status check built on the decision is the policy the
// server enforces, not a second opinion about it.
func TestBranchGateAnswersWhatItEnforces(t *testing.T) {
	t.Parallel()

	s := startWithBranchGate(t)
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	planner := s.repoAs(t, exampleTokens["planner"])

	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	// A change without a review: the check refuses it, and the land meets the
	// same refusal.
	answer, err := builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{Branch: "main", Commits: []string{commit}})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed())
	require.Equal(t, sdk.BranchNoReview, answer.GetCode())
	require.Contains(t, answer.GetReason(), "has no review")

	_, err = landAs(s.ctx, builder, "builder", "main", commit)
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no review on the branch")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	// The same change with an approved review: the check allows it, and the
	// land lands.
	review, err := builder.CreateReview(s.ctx, sdk.CreateReviewOptions{
		TargetBranch: "main",
		Head:         commit,
		Base:         base,
		Title:        "land the change",
	})
	require.NoError(t, err)
	_, err = planner.ApproveReview(s.ctx, review.GetId(), "looks good")
	require.NoError(t, err)

	answer, err = builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{Branch: "main", Commits: []string{commit}})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "reason: %s", answer.GetReason())
	require.Equal(t, base, answer.GetTip())

	landed, err := landAs(s.ctx, builder, "builder", "main", commit)
	require.NoError(t, err)
	require.NotEmpty(t, landed.LandedCommitID)
	require.NotEqual(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	// A push: the check refuses a direct update, and the push is refused with
	// the same code.
	answer, err = builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{Branch: "main", Direct: true, Commits: []string{commit}})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed())
	require.Equal(t, sdk.BranchDirectUpdateDenied, answer.GetCode())

	clientPath := filepath.Join(t.TempDir(), "client")
	gittest.Exec(t, s.cfg, "clone", "--quiet", s.repoPath, clientPath)
	require.NoError(t, os.WriteFile(filepath.Join(clientPath, "pushed.go"), []byte("package main\n"), 0o644))
	gittest.Exec(t, s.cfg, "-C", clientPath, "add", "pushed.go")
	gittest.Exec(t, s.cfg, "-C", clientPath, "-c", "user.name=builder", "-c", "user.email=builder@example.com", "commit", "--quiet", "-m", "pushed")
	out, ok := agentPush(t, s, clientPath, exampleTokens["builder"], "builder", "HEAD:refs/heads/main")
	require.False(t, ok, out)
	require.Contains(t, out, "update aborted by the reference-transaction hook")

	// The rules the check is made with are readable, and they are the rules of
	// the server's configuration.
	rules, err := builder.BranchRules(s.ctx)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.Equal(t, "main", rules[0].GetName())
	require.True(t, rules[0].GetRequireReview())
	require.False(t, rules[0].GetAllowDirectUpdate())
}
