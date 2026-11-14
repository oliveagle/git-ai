package sdk

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// BranchRule is the management policy of one branch: what may update it, and
// what a change must carry to land on it.
type BranchRule = gitalypb.BranchRule

// BranchDecision is the answer of the server to "may this change land on this
// branch?": the outcome, the reason, and the state the answer was made with.
type BranchDecision = gitalypb.GetBranchDecisionResponse

// The codes of a decision, for a machine to read. They are the codes of the
// server's decision (internal/graft/branchgate), not an invention of the SDK.
const (
	// BranchAllowed is the code of a change the policy allows.
	BranchAllowed = "allowed"
	// BranchNotManaged is the code of a branch without a rule.
	BranchNotManaged = "not_managed"
	// BranchDirectUpdateDenied is the code of an update that is not a land of
	// a branch that only a land may update.
	BranchDirectUpdateDenied = "direct_update_denied"
	// BranchDeletionDenied is the code of the deletion of a managed branch.
	BranchDeletionDenied = "deletion_denied"
	// BranchCreationDenied is the code of the creation of a managed branch by
	// a client.
	BranchCreationDenied = "creation_denied"
	// BranchNoBranch is the code of an update of a branch that does not exist.
	BranchNoBranch = "no_branch"
	// BranchNoReview is the code of a change that has no review on the
	// branch.
	BranchNoReview = "no_review"
	// BranchStaleReview is the code of a review that is not based on the tip
	// of the branch.
	BranchStaleReview = "stale_review"
	// BranchNotApproved is the code of a review the approval rules do not
	// approve.
	BranchNotApproved = "not_approved"
	// BranchNotEnoughApprovals is the code of a review with fewer approvals
	// than the rule of the branch asks for.
	BranchNotEnoughApprovals = "not_enough_approvals"
	// BranchNotVerified is the code of a branch whose verification checkpoint
	// does not cover its tip.
	BranchNotVerified = "not_verified"
	// BranchResultUnknown is the code of a branch whose rule asks for a
	// verified result and a caller that did not name the commit the update
	// produces.
	BranchResultUnknown = "result_unknown"
	// BranchStaleResult is the code of a branch whose rule asks for a
	// verified result and a result the branch would no longer write: the
	// branch moved after the result was computed, and the land produces
	// another commit.
	BranchStaleResult = "stale_result"
	// BranchNotFullTree is the code of a branch whose rule asks for the whole
	// tree, and a checkpoint that certifies no tree or only part of one: a
	// suite that ran on part of the tree is no evidence about the rest.
	BranchNotFullTree = "not_full_tree"
)

// BranchDecisionOptions asks whether changes may land on a branch.
type BranchDecisionOptions struct {
	// Branch is the branch the changes would land on, without the refs/heads/
	// prefix.
	Branch string
	// Commits are the changes that would land: the commits the reviews are
	// about. Empty asks about the state of the branch alone.
	Commits []string
	// Direct asks about an update that is not a land, such as a push.
	Direct bool
	// Result is the commit the update would produce, when the caller knows it:
	// the commit a dry-run land returned (LandOptions.DryRun). The
	// verification checkpoint may cover that commit instead of the tip of the
	// branch, which is how a merge queue verifies its result before it lands.
	Result string
}

// BranchDecision asks the server whether the changes may land on the branch,
// with the policy the server enforces and the state of the branch. The answer
// is the one the reference-transaction hook makes: a CI can use it as the
// status check of a merge request, and the server refuses the land exactly
// when the answer is no.
func (r *Repo) BranchDecision(ctx context.Context, opts BranchDecisionOptions) (*BranchDecision, error) {
	branch := strings.TrimPrefix(opts.Branch, "refs/heads/")
	if branch == "" {
		return nil, status.Error(codes.InvalidArgument, "branch is required")
	}

	return gitalypb.NewBranchPolicyServiceClient(r.client.conn).GetBranchDecision(ctx, &gitalypb.GetBranchDecisionRequest{
		Repository: r.repository(),
		Branch:     branch,
		Commits:    opts.Commits,
		Direct:     opts.Direct,
		Result:     opts.Result,
	})
}

// BranchRules returns the branch rules the server enforces. A CI prints them
// to show the policy it is the status check of.
func (r *Repo) BranchRules(ctx context.Context) ([]*BranchRule, error) {
	resp, err := gitalypb.NewBranchPolicyServiceClient(r.client.conn).ListBranchRules(ctx, &gitalypb.ListBranchRulesRequest{
		Repository: r.repository(),
	})
	if err != nil {
		return nil, err
	}
	return resp.GetRules(), nil
}
