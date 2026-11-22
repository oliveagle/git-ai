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
	"sort"
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

	policy := s.policy.Current()

	// The rule says which suites the branch requires: the state says which
	// of them the checkpoint certifies, and on which commit.
	var required []string
	if rule := policy.Rule(branch); rule != nil {
		required = rule.RequireSuites
	}

	state, err := s.source.BranchState(ctx, req.GetRepository(), branch, required)
	if err != nil {
		return nil, structerr.NewInternal("reading the state of the branch %q: %w", branch, err)
	}

	// A result the branch would no longer write is not a result: the branch
	// moved after it was computed, and a land now writes another commit. The
	// caller is a CI that asks whether the commit it tested may land, so it
	// has to be told that the branch moved rather than that nothing was
	// verified.
	stale, err := s.staleResult(ctx, req.GetRepository(), tip, req.GetResult())
	if err != nil {
		// The server cannot tell: the result may be a commit it does not know.
		// The decision then says what it knows, and the land decides the rest.
		s.deps.GetLogger().WithError(err).WithField("branch", branch).Warn("graft: the staleness of the result of a branch decision is unknown")
	}

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
		Checkpoint:  state.Checkpoint,
		Result:      req.GetResult(),
		ResultStale: stale,
		Direct:      req.GetDirect(),
		// The tree the checkpoint certifies, and the tree of the commit it
		// names: the checkpoint certifies the whole commit when they are the
		// same tree, which is what a rule that asks for the whole tree wants.
		CertifiedTree:  state.CertifiedTree,
		CheckpointTree: state.CheckpointTree,
		// The suites the checkpoint certifies, among the ones the rule
		// requires: the answer says which of them the rule is missing.
		CheckpointSuites: state.Suites,
	})

	return &gitalypb.GetBranchDecisionResponse{
		Branch:           branch,
		Allowed:          decision.Allowed,
		Code:             decision.Code,
		Reason:           decision.Reason,
		Managed:          decision.Rule != nil,
		Tip:              tip,
		Checkpoint:       state.Checkpoint,
		CheckpointTree:   state.CheckpointTree,
		CertifiedTree:    state.CertifiedTree,
		CheckpointSuites: certifiedSuites(state.Suites),
		Reviews:          uint32(len(state.Reviews)),
		Rule:             protoRule(decision.Rule),
		Change:           decision.Change,
	}, nil
}

// staleResult reports whether the result the caller named is one the branch
// would no longer write. The tip of the branch must be an ancestor of the
// result for the result to be the commit a land writes: a result computed on
// another tip is stale, and the land of it produces another commit.
//
// A result the repository does not know is not stale: the server cannot tell,
// and the decision says what it knows.
func (s *server) staleResult(ctx context.Context, repo *gitalypb.Repository, tip, result string) (bool, error) {
	if tip == "" || result == "" {
		return false, nil
	}

	local := s.deps.GetRepositoryFactory().Build(repo)
	ancestor, err := local.IsAncestor(ctx, git.Revision(tip), git.Revision(result))
	if err != nil {
		return false, err
	}

	return !ancestor, nil
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
// certifiedSuites returns the names of the suites the checkpoint certifies, in
// the order of the names, so that the answer of the server does not depend on
// the order the suites were read in.
func certifiedSuites(suites map[string]string) []string {
	if len(suites) == 0 {
		return nil
	}
	names := make([]string, 0, len(suites))
	for name := range suites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

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
		RequireFullTree:       rule.RequireFullTree,
		RequireSuites:         rule.RequireSuites,
	}
}
