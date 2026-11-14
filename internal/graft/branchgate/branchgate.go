// Package branchgate decides whether an update of a branch is allowed by the
// branch management policy of the server ([[graft.branch]]). It is pure Go:
// the caller supplies the rules, the state of the branch and the reviews, and
// gets back the decision. The reference-transaction hook decides every update
// of a branch with it (internal/gitaly/hook), and graft gate check asks the
// same question before a land (cmd/graft). See doc/graft/BRANCHES.md.
package branchgate

import (
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/review/rules"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// The codes of a decision, for a machine to read.
const (
	// CodeAllowed is the code of an update the policy allows.
	CodeAllowed = "allowed"
	// CodeNotManaged is the code of an update of a branch without a rule.
	CodeNotManaged = "not_managed"
	// CodeDirectUpdateDenied is the code of an update that is not a land of a
	// branch that only a land may update.
	CodeDirectUpdateDenied = "direct_update_denied"
	// CodeDeletionDenied is the code of the deletion of a managed branch.
	CodeDeletionDenied = "deletion_denied"
	// CodeCreationDenied is the code of the creation of a managed branch by a
	// client.
	CodeCreationDenied = "creation_denied"
	// CodeNoBranch is the code of an update of a branch that does not exist.
	CodeNoBranch = "no_branch"
	// CodeNoReview is the code of a change that has no review.
	CodeNoReview = "no_review"
	// CodeStaleReview is the code of a review that is not based on the tip of
	// the branch.
	CodeStaleReview = "stale_review"
	// CodeNotApproved is the code of a review that the approval rules do not
	// approve.
	CodeNotApproved = "not_approved"
	// CodeNotEnoughApprovals is the code of a review with fewer approvals
	// than the rule of the branch asks for.
	CodeNotEnoughApprovals = "not_enough_approvals"
	// CodeNotVerified is the code of a branch whose verification checkpoint
	// does not cover its tip.
	CodeNotVerified = "not_verified"
	// CodeResultUnknown is the code of a rule that demands the checkpoint cover
	// the commit the update produces, and a caller that did not name it.
	CodeResultUnknown = "result_unknown"
	// CodeStaleResult is the code of a rule that demands the checkpoint cover
	// the commit the update produces, and a result the branch would no longer
	// write: the branch moved after the result was computed.
	CodeStaleResult = "stale_result"
)

// Input is the state one decision is made with.
type Input struct {
	// Rules are the branch management rules of the server.
	Rules []config.GraftBranchRule
	// ReviewRules are the approval rules of [graft.review], as the server
	// configures them (config.Graft.Review.RulesOrDefault).
	ReviewRules []config.GraftReviewRule
	// Branch is the branch the update writes, without the refs/heads/ prefix.
	Branch string
	// Changes are the commits the update carries into the branch: the commits
	// a land submits, or the commit a direct update writes.
	Changes []string
	// Tip is the commit the branch points at before the update. It is empty
	// when the update creates the branch.
	Tip string
	// Reviews are the reviews of the branch.
	Reviews []*gitalypb.Review
	// Checkpoint is the commit the verification checkpoint of the branch
	// points at (refs/graft/green/<branch>), empty when the branch has none.
	// It is the commit whose tree passed the verification of the branch.
	Checkpoint string
	// Result is the commit the update produces, empty when the caller does not
	// know it. The hook knows it: git hands the reference transaction the new
	// object ID. A CI knows it when it asked the server for the result of the
	// land with a dry-run and verified that commit before landing.
	Result string
	// ResultStale tells whether the commit the update produces is one the
	// branch would no longer write: the branch moved after the result was
	// computed, and a land now produces another commit. The server reads it
	// from the repository, where the tip of the branch is no longer an
	// ancestor of the result. It is false when the caller named no result.
	ResultStale bool
	// Direct tells whether the update is a direct update of the branch, such
	// as a push, rather than a land.
	Direct bool
	// Creation tells whether the update creates the branch.
	Creation bool
	// Deletion tells whether the update deletes the branch.
	Deletion bool
}

// Decision is the outcome of the decision of one update.
type Decision struct {
	// Allowed tells whether the update may go on.
	Allowed bool
	// Code names the outcome, for a machine to read.
	Code string
	// Reason explains the outcome for a human. It is empty when the update is
	// allowed.
	Reason string
	// Rule is the rule the decision was made with, nil when the branch is not
	// managed.
	Rule *config.GraftBranchRule
	// Change is the commit the decision is about, empty when it is about the
	// update as a whole.
	Change string
}

// Rule returns the rule that governs the branch, nil when the branch is not
// managed. The rule is a copy: the caller cannot change it.
func Rule(rules []config.GraftBranchRule, branch string) *config.GraftBranchRule {
	for _, rule := range rules {
		if rule.Name == branch {
			copied := rule
			return &copied
		}
	}
	return nil
}

// Decide decides whether the update of the branch may go on.
//
// A branch without a rule is not managed and its updates are allowed. A
// managed branch accepts only a land, unless its rule allows direct updates,
// and a deletion of it is always refused. A land must carry the review of
// every change it lands, approved by the approval rules of the server, based
// on the tip of the branch, unless the rule allows a stale base. When the
// rule asks for it, the verification checkpoint of the branch must cover the
// update: the tip it builds on, or the commit it produces (verified). A rule
// that asks for a verified result (RequireVerifiedResult) accepts only the
// checkpoint on the commit the update produces, which is the rule of a merge
// queue, and refuses a caller that does not name that commit. A result the
// branch would no longer write, because the branch moved after it was
// computed, is refused as stale: the land writes another commit, and no
// checkpoint covers it.
func Decide(in Input) Decision {
	rule := Rule(in.Rules, in.Branch)
	if rule == nil {
		return Decision{Allowed: true, Code: CodeNotManaged, Reason: fmt.Sprintf("the branch %q is not managed", in.Branch)}
	}

	if in.Deletion {
		return deny(rule, "", CodeDeletionDenied, "the branch %q is managed and cannot be deleted", in.Branch)
	}

	if in.Creation {
		// The branch does not exist yet, so there is no history to protect, but
		// a managed branch is created by the server and not by a client: the
		// server creates the first branch of a repository with the operator
		// credential.
		return deny(rule, "", CodeCreationDenied, "the branch %q is managed: a client may not create it", in.Branch)
	}

	if in.Direct && !rule.AllowDirectUpdate {
		return deny(rule, "", CodeDirectUpdateDenied, "the branch %q is managed: only a land may update it", in.Branch)
	}

	if in.Tip == "" {
		// The branch does not exist: there is nothing to land on.
		return deny(rule, "", CodeNoBranch, "the branch %q does not exist", in.Branch)
	}

	if rule.RequireReview || rule.RequireApprovals > 0 {
		for _, change := range in.Changes {
			decision := decideChange(rule, in, change)
			if !decision.Allowed {
				return decision
			}
		}
	}

	if rule.RequireVerifiedResult {
		// The rule of a merge queue: the branch takes a land only when the
		// checkpoint covers the commit this very update produces, so that what
		// the branch holds was tested as it will be written, not only as the
		// tip it was built on.
		if in.Result == "" {
			return deny(rule, "", CodeResultUnknown,
				"the branch %q takes only a verified result: name the commit the update produces (the result of a dry-run land) so that the checkpoint can be checked against it",
				in.Branch)
		}
		if in.ResultStale {
			// The result was computed on another tip: the land writes another
			// commit, and no checkpoint can cover it. The queue has to ask for
			// the result again, not publish a checkpoint for a commit the
			// branch will not hold.
			return deny(rule, "", CodeStaleResult,
				"the result %s was computed before the branch moved to %s: the land has to be computed again",
				in.Result, in.Tip)
		}
		if in.Checkpoint != in.Result {
			if in.Checkpoint == "" {
				return deny(rule, "", CodeNotVerified,
					"the branch %q has no verification checkpoint, and its rule requires one on %s, the commit the update produces",
					in.Branch, in.Result)
			}
			return deny(rule, "", CodeNotVerified,
				"the checkpoint of the branch %q is %s, not %s, the commit the update produces: the rule requires the checkpoint on the result of this land",
				in.Branch, in.Checkpoint, in.Result)
		}
	} else if rule.RequireVerified && !verified(in) {
		if in.Result == "" {
			return deny(rule, "", CodeNotVerified, "the verification checkpoint of the branch %q does not cover its tip %s", in.Branch, in.Tip)
		}
		return deny(rule, "", CodeNotVerified,
			"the verification checkpoint of the branch %q covers neither its tip %s nor the commit %s the update produces",
			in.Branch, in.Tip, in.Result)
	}

	return allow(rule, "", "")
}

// verified reports whether the verification checkpoint of the branch covers the
// update: the tip it builds on, or the commit it produces.
//
// A checkpoint on the produced commit is the statement of a CI that ran the
// verification on the result of this very update, which is what a merge queue
// does: it asks the server what the land would produce, tests that commit, and
// publishes it as the checkpoint before the land. The branch is then green by
// construction once the update is written, so the update is as verified as one
// that lands on a green tip.
func verified(in Input) bool {
	if in.Checkpoint == "" {
		return false
	}
	if in.Checkpoint == in.Tip {
		return true
	}
	return in.Result != "" && in.Checkpoint == in.Result
}

// decideChange decides whether one change may land on the branch.
func decideChange(rule *config.GraftBranchRule, in Input, change string) Decision {
	review := findReview(in.Reviews, in.Branch, change)
	if review == nil {
		return deny(rule, change, CodeNoReview, "the change %s has no review on the branch %q", change, in.Branch)
	}

	if !rule.AllowStaleBase && review.GetBaseCommitId() != in.Tip {
		return deny(rule, change, CodeStaleReview,
			"the review of the change %s is based on %s, not on the tip %s of the branch %q",
			change, review.GetBaseCommitId(), in.Tip, in.Branch)
	}

	requirements, approved := rules.Evaluate(in.ReviewRules, review.GetFiles(), review.GetAuthor(), review.GetVerdicts())
	if !approved {
		return deny(rule, change, CodeNotApproved, "the review of the change %s is not approved: %s", change, describeRequirements(requirements))
	}

	if rule.RequireApprovals > 0 {
		approvals := countApprovals(review.GetVerdicts())
		if approvals < int(rule.RequireApprovals) {
			return deny(rule, change, CodeNotEnoughApprovals,
				"the review of the change %s has %d approvals, %d are required",
				change, approvals, rule.RequireApprovals)
		}
	}

	return allow(rule, change, "")
}

// findReview returns the review of the change on the branch, nil when there
// is none. The newest review wins: the reviews of a change are ordered by
// their update time, and two reviews of one change are told apart by their
// identifier, which grows with every review of the repository.
func findReview(reviews []*gitalypb.Review, branch, change string) *gitalypb.Review {
	var found *gitalypb.Review
	for _, review := range reviews {
		if string(review.GetTargetBranch()) != branch || review.GetHeadCommitId() != change {
			continue
		}
		if found == nil || newer(review, found) {
			found = review
		}
	}
	return found
}

// newer reports whether review is more recent than other. The update time
// decides, and the identifier breaks a tie: the identifiers of the reviews of
// a repository are decimal counters that grow with every review.
func newer(review, other *gitalypb.Review) bool {
	a, b := review.GetUpdatedAt(), other.GetUpdatedAt()
	if a == nil || b == nil {
		return a != nil
	}
	if !a.AsTime().Equal(b.AsTime()) {
		return a.AsTime().After(b.AsTime())
	}
	return review.GetId() > other.GetId()
}

// countApprovals counts the actors whose latest current verdict approves the
// change. Only the latest verdict of an actor counts, and only when it is
// current: an approval that a later verdict replaced does not count.
func countApprovals(verdicts []*gitalypb.ReviewVerdict) int {
	latest := make(map[string]*gitalypb.ReviewVerdict, len(verdicts))
	for _, verdict := range verdicts {
		latest[verdict.GetActor()] = verdict
	}

	approvals := 0
	for _, verdict := range latest {
		if verdict.GetCurrent() && verdict.GetKind() == gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED {
			approvals++
		}
	}
	return approvals
}

// describeRequirements describes the requirements of a review that are not
// satisfied, for the message of a denial.
func describeRequirements(requirements []*gitalypb.ReviewRequirement) string {
	var unmet []string
	for _, requirement := range requirements {
		if requirement.GetSatisfied() {
			continue
		}
		unmet = append(unmet, fmt.Sprintf("%s needs %d approvals and has %d",
			requirement.GetRule(), requirement.GetRequiredApprovals(), requirement.GetApprovals()))
	}
	if len(unmet) == 0 {
		return "a reviewer requested changes"
	}
	return strings.Join(unmet, ", ")
}

// allow returns the decision of an allowed update.
func allow(rule *config.GraftBranchRule, change, format string, args ...any) Decision {
	decision := Decision{Allowed: true, Code: CodeAllowed, Rule: rule, Change: change}
	if format != "" {
		decision.Reason = fmt.Sprintf(format, args...)
	}
	return decision
}

// deny returns the decision of a refused update.
func deny(rule *config.GraftBranchRule, change, code, format string, args ...any) Decision {
	return Decision{
		Allowed: false,
		Code:    code,
		Reason:  fmt.Sprintf(format, args...),
		Rule:    rule,
		Change:  change,
	}
}
