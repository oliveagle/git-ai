package branchgate

import (
	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// Policy is the branch management policy of the server: the rules of the
// branches, and the approval rules a review is judged with.
//
// The server builds one policy and hands it both to the reference-transaction
// hook, which enforces it, and to the BranchPolicyService, which answers the
// question a CI asks. The answer a CI reads is therefore made with the very
// policy the server enforces: the two cannot drift apart.
type Policy struct {
	// Rules are the branch management rules of the server, one per managed
	// branch.
	Rules []config.GraftBranchRule
	// ReviewRules are the approval rules of [graft.review], as the server
	// configures them.
	ReviewRules []config.GraftReviewRule
}

// NewPolicy returns the branch management policy of a configuration.
func NewPolicy(graft config.Graft) *Policy {
	return &Policy{
		Rules:       graft.Branches,
		ReviewRules: graft.Review.RulesOrDefault(),
	}
}

// Rule returns the rule that governs the branch, nil when the branch is not
// managed.
func (p *Policy) Rule(branch string) *config.GraftBranchRule {
	if p == nil {
		return nil
	}
	return Rule(p.Rules, branch)
}

// Empty tells whether the policy governs no branch at all.
func (p *Policy) Empty() bool {
	return p == nil || len(p.Rules) == 0
}
