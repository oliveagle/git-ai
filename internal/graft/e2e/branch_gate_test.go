package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

	// The same update through an RPC is refused too: the gate decides every
	// update of the branch, not only the one a push makes.
	err = builder.UpdateReference(s.ctx, "refs/heads/main", base, pushed)
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

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

// TestBranchGateVerifiesTheResultOfALand checks the loop a merge queue runs on
// a branch that only takes verified lands: the server says what the land would
// produce, the CI verifies that commit and publishes it as the checkpoint of
// the branch, and the land then produces exactly that commit, so the branch is
// green the moment it is written.
func TestBranchGateVerifiesTheResultOfALand(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireVerified: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	// The branch moves on while the change is in review, so the land has to
	// replay the change onto the new tip: the commit it produces is not the one
	// the CI was given to verify.
	tip := writeMain(t, s, map[string]string{"README.md": "main\n", "NOTES.md": "notes\n"}, base)

	land := sdk.LandOptions{
		Branch:    "main",
		Commit:    commit,
		User:      sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
		Timestamp: time.Unix(1700000000, 0),
	}

	// The CI asks the server what the land would produce. Nothing of the branch
	// changes, and the commit it names is readable: the CI checks it out.
	dry := land
	dry.DryRun = true
	produced, err := builder.Land(s.ctx, dry)
	require.NoError(t, err)
	require.True(t, produced.DryRun)
	require.NotEmpty(t, produced.LandedCommitID)
	require.NotEqual(t, commit, produced.LandedCommitID, "the land rewrites the change onto the tip")
	require.Equal(t, tip, s.git(t, "rev-parse", "refs/heads/main"))
	require.Equal(t, "package main", s.git(t, "cat-file", "-p", produced.LandedCommitID+":main.go"))

	// Nothing is verified yet: the gate refuses the change and names the commit
	// that would have to be verified, which is the answer a CI prints.
	answer, err := builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed())
	require.Equal(t, sdk.BranchNotVerified, answer.GetCode())
	require.Contains(t, answer.GetReason(), produced.LandedCommitID)

	// The land itself is refused for the same reason.
	_, err = builder.Land(s.ctx, land)
	require.Error(t, err)
	require.Contains(t, err.Error(), "verification checkpoint")
	require.Equal(t, tip, s.git(t, "rev-parse", "refs/heads/main"))

	// The CI verified that commit and published it as the checkpoint of the
	// branch: the operator writes the checkpoint, an agent cannot (see
	// TestBranchGateKeepsTheCheckpointFromAgents).
	s.git(t, "update-ref", "refs/graft/green/main", produced.LandedCommitID)

	answer, err = builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "reason: %s", answer.GetReason())

	// The land produces exactly the commit the CI verified, so the branch is
	// green when the land is written.
	landed, err := builder.Land(s.ctx, land)
	require.NoError(t, err)
	require.False(t, landed.DryRun, "the land is not a dry run")
	require.Equal(t, produced.LandedCommitID, landed.LandedCommitID)
	require.Equal(t, produced.LandedCommitID, s.git(t, "rev-parse", "refs/heads/main"))
	require.Equal(t, produced.LandedCommitID, s.git(t, "rev-parse", "refs/graft/green/main"))

	// The branch is green now: the checkpoint covers its tip, and the next land
	// waits for the next verification.
	answer, err = builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{Branch: "main"})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "reason: %s", answer.GetReason())
	require.Equal(t, produced.LandedCommitID, answer.GetTip())
}

// TestBranchGateOnlyTakesAVerifiedResult checks the rule of a merge queue that
// a green tip cannot satisfy: with require_verified_result the branch takes a
// land only when the checkpoint covers the commit that land produces, so a
// change lands after its own result was verified, not on the strength of the
// tip it was built on.
func TestBranchGateOnlyTakesAVerifiedResult(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireVerifiedResult: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	// The tip is green: a verifier ran on it. A rule of require_verified alone
	// would take the next land on that.
	s.git(t, "update-ref", "refs/graft/green/main", base)

	builder := s.repoAs(t, exampleTokens["builder"])
	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	// The gate cannot answer a rule of a verified result without the commit
	// the land produces, and says so instead of guessing.
	answer, err := builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
	})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed())
	require.Equal(t, sdk.BranchResultUnknown, answer.GetCode())
	require.Equal(t, base, answer.GetCheckpoint())

	// The hook knows the result, and the checkpoint covers the tip, not it: the
	// land is refused and the branch does not move.
	land := sdk.LandOptions{
		Branch:    "main",
		Commit:    commit,
		User:      sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
		Timestamp: time.Unix(1700000000, 0),
	}
	_, err = builder.Land(s.ctx, land)
	require.Error(t, err)
	require.Contains(t, err.Error(), "the checkpoint of the branch")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	// The CI asks what the land would produce, verifies that commit and
	// publishes it as the checkpoint of the branch.
	dry := land
	dry.DryRun = true
	produced, err := builder.Land(s.ctx, dry)
	require.NoError(t, err)
	require.NotEmpty(t, produced.LandedCommitID)
	require.NotEqual(t, base, produced.LandedCommitID)

	answer, err = builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed(), "a checkpoint on the tip does not cover the result")
	require.Equal(t, sdk.BranchNotVerified, answer.GetCode())
	require.Contains(t, answer.GetReason(), produced.LandedCommitID)

	s.git(t, "update-ref", "refs/graft/green/main", produced.LandedCommitID)

	answer, err = builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "reason: %s", answer.GetReason())

	// The land writes exactly the commit the CI verified, so the branch is
	// green the moment it is written.
	landed, err := builder.Land(s.ctx, land)
	require.NoError(t, err)
	require.Equal(t, produced.LandedCommitID, landed.LandedCommitID)
	require.Equal(t, produced.LandedCommitID, s.git(t, "rev-parse", "refs/heads/main"))
	require.Equal(t, produced.LandedCommitID, s.git(t, "rev-parse", "refs/graft/green/main"))
}

// TestBranchGateTakesOneResultPerLand checks a rule of require_verified_result
// under concurrency: two lands of one branch arrive at once, the land lanes may
// run them as one batch, and the checkpoint of the branch covers the result of
// one of them. Whatever the lane does with them, the branch takes the change
// whose result was verified and refuses the other: a batch writes a commit no
// queue named, so one checkpoint cannot let two changes in, and the branch is
// green at rest once the lane is quiet.
func TestBranchGateTakesOneResultPerLand(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireVerifiedResult: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	covered, err := commitFile(s.ctx, builder, "builder", "feature-a", base, "a.txt", "a\n")
	require.NoError(t, err)
	other, err := commitFile(s.ctx, builder, "builder", "feature-b", base, "b.txt", "b\n")
	require.NoError(t, err)

	// The queue verifies the result of one change and publishes it. The date is
	// part of the identity of the commit, so the land that the queue runs writes
	// the commit the dry run named.
	lands := []sdk.LandOptions{
		{
			Branch:    "main",
			Commit:    covered,
			User:      sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
			Timestamp: time.Unix(1700000100, 0),
		},
		{
			Branch:    "main",
			Commit:    other,
			User:      sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
			Timestamp: time.Unix(1700000200, 0),
		},
	}

	dry := lands[0]
	dry.DryRun = true
	produced, err := builder.Land(s.ctx, dry)
	require.NoError(t, err)
	require.NotEmpty(t, produced.LandedCommitID)
	require.Equal(t, base, produced.OldTip, "the dry run names the tip it built on")
	s.git(t, "update-ref", "refs/graft/green/main", produced.LandedCommitID)

	// Both lands arrive at once. The result of the second one is not what the
	// checkpoint covers, whichever order the lane takes them in.
	errs := make([]error, len(lands))
	var wg sync.WaitGroup
	for i := range lands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = builder.Land(s.ctx, lands[i])
		}(i)
	}
	wg.Wait()

	require.NoError(t, errs[0], "the change whose result the checkpoint covers lands")
	require.Error(t, errs[1], "a change whose result nobody verified does not land")
	require.Contains(t, errs[1].Error(), produced.LandedCommitID,
		"the refusal names the checkpoint, so the queue knows what to re-run")

	tip := s.git(t, "rev-parse", "refs/heads/main")
	require.Equal(t, produced.LandedCommitID, tip, "the branch holds the commit the queue tested")
	require.Equal(t, tip, s.git(t, "rev-parse", "refs/graft/green/main"),
		"the branch is green at rest: no commit reached it without a checkpoint")

	// The refused change waits for a queue that verifies the commit the land
	// would produce on the tip the queue left behind.
	again := lands[1]
	again.Timestamp = time.Unix(1700000300, 0)
	dry = again
	dry.DryRun = true
	next, err := builder.Land(s.ctx, dry)
	require.NoError(t, err)
	answer, err := builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{other},
		Result:  next.LandedCommitID,
	})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed())
	require.Equal(t, sdk.BranchNotVerified, answer.GetCode())

	// The queue publishes that result, and the land writes it.
	s.git(t, "update-ref", "refs/graft/green/main", next.LandedCommitID)
	landed, err := builder.Land(s.ctx, again)
	require.NoError(t, err)
	require.Equal(t, next.LandedCommitID, landed.LandedCommitID)
	require.Equal(t, next.LandedCommitID, s.git(t, "rev-parse", "refs/heads/main"))
}

// TestBranchGateSaysAStaleResult checks what a queue reads when the branch
// moved after it computed the result: the result is not the commit the branch
// would take any more, so the answer says the result is stale, and the queue
// computes it again instead of publishing a checkpoint for a commit the branch
// will not hold.
func TestBranchGateSaysAStaleResult(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireVerifiedResult: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	commit, err := commitFile(s.ctx, builder, "builder", "feature", base, "main.go", "package main\n")
	require.NoError(t, err)

	// The queue asks what the land would produce, and the CI tests that commit.
	land := sdk.LandOptions{
		Branch:    "main",
		Commit:    commit,
		User:      sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
		Timestamp: time.Unix(1700000000, 0),
	}
	dry := land
	dry.DryRun = true
	produced, err := builder.Land(s.ctx, dry)
	require.NoError(t, err)
	require.NotEmpty(t, produced.LandedCommitID)

	// The CI publishes the checkpoint of the result it tested.
	s.git(t, "update-ref", "refs/graft/green/main", produced.LandedCommitID)

	answer, err := builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "reason: %s", answer.GetReason())

	// The branch moves: another change lands on it while the CI was testing.
	other, err := commitFile(s.ctx, builder, "builder", "feature-other", base, "other.go", "package main\n")
	require.NoError(t, err)
	s.git(t, "update-ref", "refs/heads/main", other)

	// The result the CI tested is not the one the branch would take any more:
	// the answer says so, and the queue computes the result again.
	answer, err = builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{
		Branch:  "main",
		Commits: []string{commit},
		Result:  produced.LandedCommitID,
	})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed(), "reason: %s", answer.GetReason())
	require.Equal(t, sdk.BranchStaleResult, answer.GetCode())
	require.Contains(t, answer.GetReason(), produced.LandedCommitID)
}

// branch is published by an operator and only read by agents. A checkpoint
// that the party being gated can move says nothing about what was verified.
func TestBranchGateKeepsTheCheckpointFromAgents(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireVerified: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])

	// An agent token may not move the checkpoint of the branch.
	err := builder.UpdateReference(s.ctx, "refs/graft/green/main", "", base)
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// The branch has no checkpoint, so no land is verified.
	answer, err := builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{Branch: "main"})
	require.NoError(t, err)
	require.False(t, answer.GetAllowed())
	require.Equal(t, sdk.BranchNotVerified, answer.GetCode())
	require.Empty(t, answer.GetCheckpoint())

	// The operator publishes it, and the gate reads what the operator wrote.
	s.git(t, "update-ref", "refs/graft/green/main", base)
	answer, err = builder.BranchDecision(s.ctx, sdk.BranchDecisionOptions{Branch: "main"})
	require.NoError(t, err)
	require.True(t, answer.GetAllowed(), "reason: %s", answer.GetReason())
	require.Equal(t, base, answer.GetCheckpoint())
}
