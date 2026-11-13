package e2e

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/sdk"
)

// TestBranchRulesReloadOnSignal reloads the branch management policy of a running server with
// SIGUSR1: a rule put in force refuses a land the server allowed a moment before, and a rule taken
// out of force allows it again. The rules are the ones in the configuration file, read again, not a
// copy the server started with.
func TestBranchRulesReloadOnSignal(t *testing.T) {
	t.Parallel()

	n := newRealNode(t)
	n.start(t)
	rel := n.createRepository(t)
	worker := n.agent(t, rel)
	operator := n.operator(t, rel)

	// The repository has a branch to manage. The worker writes it: the example policy of a node
	// lets the workers update main, and no rule manages it yet.
	base, err := commitFile(n.ctx, worker, workerName(0), "graft/seed", "", "README.md", "main\n")
	require.NoError(t, err)
	_, err = landAs(n.ctx, worker, workerName(0), "main", base)
	require.NoError(t, err)
	tip, err := worker.ResolveRevision(n.ctx, "main")
	require.NoError(t, err)

	// rewrite replaces the configuration file with one whose branch rules are rules.
	rewrite := func(rules []config.GraftBranchRule) {
		cfg := n.cfg
		cfg.Graft.Branches = rules
		require.NoError(t, os.Rename(testcfg.WriteTemporaryGitalyConfigFile(t, cfg), n.cfgPath))
	}
	// waitForRules sends the reload signal and waits until the server answers with n rules: the
	// answer a CI reads is made with the policy the hook enforces.
	waitForRules := func(count int) {
		require.NoError(t, n.cmd.Process.Signal(syscall.SIGUSR1))
		require.Eventually(t, func() bool {
			rules, err := worker.BranchRules(n.ctx)
			return err == nil && len(rules) == count
		}, time.Minute, 10*time.Millisecond, "the server did not answer with %d branch rules\n%s", count, n.log(t))
	}

	// A change of the worker, ready to land, is allowed while main is not managed.
	change, err := commitFile(n.ctx, worker, workerName(0), "graft/change", tip, "change.txt", "change\n")
	require.NoError(t, err)
	_, err = landAs(n.ctx, worker, workerName(0), "main", change)
	require.NoError(t, err, "a branch without a rule is not managed")
	managed, err := worker.ResolveRevision(n.ctx, "main")
	require.NoError(t, err)

	// The rule is put in force without a restart: the next land of the same kind is refused.
	rewrite([]config.GraftBranchRule{{Name: "main", RequireReview: true}})
	waitForRules(1)
	require.EqualValues(t, 2, metricOrZero(t, n, "graft_branch_policy_generation"))
	require.NotContains(t, n.log(t), "they need a restart", "the branch rules are applied by the reload")
	require.Contains(t, n.log(t), "branch policy reloaded")

	refused, err := commitFile(n.ctx, worker, workerName(0), "graft/change-2", managed, "change2.txt", "change2\n")
	require.NoError(t, err)
	_, err = landAs(n.ctx, worker, workerName(0), "main", refused)
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no review")
	require.Equal(t, managed, n.git(t, rel, "rev-parse", "refs/heads/main"), "the refused land moved nothing")

	// The rule is taken out of force: the same land is allowed again.
	rewrite(nil)
	waitForRules(0)
	require.EqualValues(t, 3, metricOrZero(t, n, "graft_branch_policy_generation"))

	landed, err := landAs(n.ctx, worker, workerName(0), "main", refused)
	require.NoError(t, err)
	require.Equal(t, landed.LandedCommitID, n.git(t, rel, "rev-parse", "refs/heads/main"))

	// The service and the hook decide with one policy: the answer the CI would read is the one
	// the land got.
	decision, err := operator.BranchDecision(n.ctx, sdk.BranchDecisionOptions{Branch: "main", Commits: []string{refused}})
	require.NoError(t, err)
	require.True(t, decision.GetAllowed(), decision.GetReason())
}
