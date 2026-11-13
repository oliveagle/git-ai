// Package branchpolicy implements the BranchPolicyService: the branch
// management policy of the server, as a service.
//
// The reference-transaction hook enforces the policy on the updates of a
// managed branch (internal/gitaly/hook). This service answers the question a
// CI asks before it lands: may this change land on this branch? It decides
// with the policy the hook enforces and with the state of the branch the hook
// reads, so that the answer of the CI's status check and the refusal of the
// server cannot disagree. See doc/graft/BRANCHES.md.
package branchpolicy

import (
	"context"
	"errors"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/graft/branchgate"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type server struct {
	gitalypb.UnimplementedBranchPolicyServiceServer
	deps *service.Dependencies
	// policy is the store of the policy the gate enforces, the one the hook
	// decides with. It is read for every decision, so that a reload of the
	// rules is what the next answer is made with.
	policy *branchgate.Store
	// source reads the state of a branch: its reviews and its verification
	// checkpoint. It is the source the hook decides with.
	source hook.BranchGateSource
}

// NewServer creates a new instance of the BranchPolicyService server.
func NewServer(deps *service.Dependencies) gitalypb.BranchPolicyServiceServer {
	return newServer(deps)
}

func newServer(deps *service.Dependencies) *server {
	policy := deps.GraftBranchPolicy
	if policy == nil {
		policy = branchgate.NewStore(deps.GetCfg().Graft)
	}

	return &server{
		deps:   deps,
		policy: policy,
		source: hook.NewBranchGateSource(deps.GetGraftStorage(), deps.GetRepositoryFactory()),
	}
}

// GetBranchDecision asks whether a change may land on a branch, with the rules
// the server enforces and the state of the branch.
func (s *server) GetBranchDecision(ctx context.Context, req *gitalypb.GetBranchDecisionRequest) (*gitalypb.GetBranchDecisionResponse, error) {
	branch, err := branchName(req.GetBranch())
	if err != nil {
		return nil, err
	}

	tip, err := s.tip(ctx, req.GetRepository(), branch)
	if err != nil {
		return nil, err
	}

	state, err := s.source.BranchState(ctx, req.GetRepository(), branch)
	if err != nil {
		return nil, structerr.NewInternal("reading the state of the branch %q: %w", branch, err)
	}

	policy := s.policy.Current()
	decision := branchgate.Decide(branchgate.Input{
		Rules:       policy.Rules,
		ReviewRules: policy.ReviewRules,
		Branch:      branch,
		Changes:     req.GetCommits(),
		Tip:         tip,
		Reviews:     state.Reviews,
		// The checkpoint covers the update when it points at the tip the
		// change builds on, or at the commit the land would produce when the
		// caller knows it (a dry-run land it verified).
		Checkpoint: state.Checkpoint,
		Result:     req.GetResult(),
		Direct:     req.GetDirect(),
	})

	return &gitalypb.GetBranchDecisionResponse{
		Branch:     branch,
		Allowed:    decision.Allowed,
		Code:       decision.Code,
		Reason:     decision.Reason,
		Managed:    decision.Rule != nil,
		Tip:        tip,
		Checkpoint: state.Checkpoint,
		Reviews:    uint32(len(state.Reviews)),
		Rule:       protoRule(decision.Rule),
		Change:     decision.Change,
	}, nil
}

// ListBranchRules lists the branch rules the server enforces.
func (s *server) ListBranchRules(ctx context.Context, req *gitalypb.ListBranchRulesRequest) (*gitalypb.ListBranchRulesResponse, error) {
	resp := &gitalypb.ListBranchRulesResponse{}
	policy := s.policy.Current()
	for i := range policy.Rules {
		resp.Rules = append(resp.Rules, protoRule(&policy.Rules[i]))
	}
	return resp, nil
}

// tip returns the commit the branch points at, empty when the branch does not
// exist. The branch is read by its exact name, so that a branch named like
// another reference is never taken for it.
func (s *server) tip(ctx context.Context, repo *gitalypb.Repository, branch string) (string, error) {
	local := s.deps.GetRepositoryFactory().Build(repo)
	oid, err := local.ResolveReferenceExactly(ctx, git.ReferenceName("refs/heads/"+branch))
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			// A branch that does not exist has no tip: the decision refuses
			// the change, and says why.
			return "", nil
		}
		return "", structerr.NewInternal("resolving the tip of the branch %q: %w", branch, err)
	}
	return oid.String(), nil
}

// branchName returns the branch the request names, without the refs/heads/
// prefix. It refuses a name that is a reference, so that the branch of a rule
// is never confused with another reference.
func branchName(name string) (string, error) {
	name = strings.TrimSpace(name)
	branch := strings.TrimPrefix(name, "refs/heads/")
	if branch == "" {
		return "", structerr.NewInvalidArgument("branch is required")
	}
	if strings.HasPrefix(branch, "refs/") || branch == "HEAD" {
		return "", structerr.NewInvalidArgument("branch %q: want a branch name, not a reference", name)
	}
	return branch, nil
}

// protoRule returns the rule as the API carries it, nil when the branch is not
// managed.
func protoRule(rule *config.GraftBranchRule) *gitalypb.BranchRule {
	if rule == nil {
		return nil
	}

	return &gitalypb.BranchRule{
		Name:              rule.Name,
		RequireReview:     rule.RequireReview,
		RequireApprovals:  rule.RequireApprovals,
		AllowStaleBase:    rule.AllowStaleBase,
		AllowDirectUpdate: rule.AllowDirectUpdate,
		RequireVerified:   rule.RequireVerified,
		// The proto carries the stricter rule of a merge queue too, so a CI
		// reads the rules the server enforces, not a subset of them.
		RequireVerifiedResult: rule.RequireVerifiedResult,
	}
}
