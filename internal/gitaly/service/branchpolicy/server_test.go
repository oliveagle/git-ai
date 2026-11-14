package branchpolicy

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/gitaly/service/review"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// harness runs a BranchPolicyService against a repository with a policy.
type harness struct {
	t        testing.TB
	ctx      context.Context
	cfg      config.Cfg
	repo     *gitalypb.Repository
	repoPath string
	policy   gitalypb.BranchPolicyServiceClient
	reviews  gitalypb.ReviewServiceClient
}

// newHarness starts a server that governs the branches of the policy, and a
// repository to ask about.
func newHarness(t *testing.T, rules ...config.GraftBranchRule) *harness {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Branches = rules
	h := &harness{t: t, ctx: ctx}

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterBranchPolicyServiceServer(srv, NewServer(deps))
		gitalypb.RegisterReviewServiceServer(srv, review.NewServer(deps))
		// gittest.CreateRepository goes through the RepositoryService.
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})
	h.cfg = cfg

	conn, err := client.New(ctx, cfg.SocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })
	h.policy = gitalypb.NewBranchPolicyServiceClient(conn)
	h.reviews = gitalypb.NewReviewServiceClient(conn)

	h.repo, h.repoPath = gittest.CreateRepository(t, ctx, cfg)
	return h
}

// write writes a commit with the contents, on top of parent (none when it is
// empty), on the branch when one is named.
func (h *harness) write(parent git.ObjectID, branch string, contents map[string]string) git.ObjectID {
	h.t.Helper()

	var entries []gittest.TreeEntry
	for path, content := range contents {
		entries = append(entries, gittest.TreeEntry{Mode: "100644", Path: path, Content: content})
	}

	opts := []gittest.WriteCommitOption{gittest.WithTreeEntries(entries...)}
	if parent != "" {
		opts = append(opts, gittest.WithParents(parent))
	}
	if branch != "" {
		opts = append(opts, gittest.WithBranch(branch))
	}
	return gittest.WriteCommit(h.t, h.cfg, h.repoPath, opts...)
}

// tip returns the commit the branch points at.
func (h *harness) tip(branch string) git.ObjectID {
	h.t.Helper()

	stdout := gittest.Exec(h.t, h.cfg, "-C", h.repoPath, "rev-parse", "refs/heads/"+branch)
	return git.ObjectID(strings.TrimSpace(string(stdout)))
}

// checkpoint moves the verification checkpoint of the branch to the commit.
func (h *harness) checkpoint(branch string, commit git.ObjectID) {
	h.t.Helper()

	gittest.Exec(h.t, h.cfg, "-C", h.repoPath, "update-ref", "refs/graft/green/"+branch, commit.String())
}

// review opens a review of the head on the branch.
func (h *harness) review(branch string, head git.ObjectID) {
	h.t.Helper()

	_, err := h.reviews.CreateReview(h.ctx, &gitalypb.CreateReviewRequest{
		Repository:   h.repo,
		TargetBranch: []byte(branch),
		HeadCommitId: head.String(),
		Title:        []byte("a change"),
	})
	require.NoError(h.t, err)
}

// decide asks the service about the changes on the branch.
func (h *harness) decide(branch string, commits ...string) *gitalypb.GetBranchDecisionResponse {
	h.t.Helper()

	resp, err := h.policy.GetBranchDecision(h.ctx, &gitalypb.GetBranchDecisionRequest{
		Repository: h.repo,
		Branch:     branch,
		Commits:    commits,
	})
	require.NoError(h.t, err)
	return resp
}

// decideAbout asks the service about a change and the commit its land would
// produce, the way a merge queue does after a dry run.
func (h *harness) decideAbout(branch, commit, result string) *gitalypb.GetBranchDecisionResponse {
	h.t.Helper()

	resp, err := h.policy.GetBranchDecision(h.ctx, &gitalypb.GetBranchDecisionRequest{
		Repository: h.repo,
		Branch:     branch,
		Commits:    []string{commit},
		Result:     result,
	})
	require.NoError(h.t, err)
	return resp
}

func requireDecision(t testing.TB, resp *gitalypb.GetBranchDecisionResponse, code string) {
	t.Helper()

	require.Equal(t, code, resp.GetCode(), "reason: %s", resp.GetReason())
	require.Equal(t, code == "allowed" || code == "not_managed", resp.GetAllowed(), "reason: %s", resp.GetReason())
}

// TestGetBranchDecision_Review checks the review part of the policy: a change
// without a review does not land, a reviewed one does, and a review whose base
// is no longer the tip of the branch does not.
func TestGetBranchDecision_Review(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftBranchRule{Name: "main", RequireReview: true})
	base := h.write("", "main", map[string]string{"README.md": "# readme\n"})
	head := h.write(base, "", map[string]string{"docs.md": "docs\n"})

	// The branch alone: there is no change to check, and the branch is
	// allowed.
	resp := h.decide("main")
	requireDecision(t, resp, "allowed")
	require.True(t, resp.GetManaged())
	require.Equal(t, base.String(), resp.GetTip())
	require.Equal(t, "main", resp.GetBranch())
	require.NotNil(t, resp.GetRule(), "the rule that decided comes with the answer")

	// A change without a review does not land.
	resp = h.decide("main", head.String())
	requireDecision(t, resp, "no_review")
	require.Equal(t, head.String(), resp.GetChange())

	// A review of a documentation change needs no approval.
	h.review("main", head)
	resp = h.decide("main", head.String())
	requireDecision(t, resp, "allowed")
	require.Equal(t, uint32(1), resp.GetReviews())

	// The review is based on the tip it was created against: when the branch
	// moves, the review no longer covers what the branch holds.
	h.write(base, "main", map[string]string{"other.md": "other\n"})
	resp = h.decide("main", head.String())
	requireDecision(t, resp, "stale_review")

	// A branch without a rule is not managed.
	resp = h.decide("other")
	requireDecision(t, resp, "not_managed")
	require.Nil(t, resp.GetRule())
}

// TestGetBranchDecision_Checkpoint checks that a rule that asks for a verified
// branch refuses the change until the checkpoint of the branch covers its tip.
func TestGetBranchDecision_Checkpoint(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftBranchRule{Name: "main", RequireVerified: true})
	tip := h.write("", "main", map[string]string{"a.txt": "a\n"})

	resp := h.decide("main")
	requireDecision(t, resp, "not_verified")
	require.Empty(t, resp.GetCheckpoint())

	// The checkpoint of the branch covers it: the branch is green.
	h.checkpoint("main", tip)
	resp = h.decide("main")
	requireDecision(t, resp, "allowed")
	require.Equal(t, tip.String(), resp.GetCheckpoint())

	// The branch moved: the checkpoint no longer covers its tip.
	moved := h.write(tip, "main", map[string]string{"b.txt": "b\n"})
	resp = h.decide("main")
	requireDecision(t, resp, "not_verified")
	require.Equal(t, moved.String(), resp.GetTip())

	h.checkpoint("main", moved)
	requireDecision(t, h.decide("main"), "allowed")
}

// TestGetBranchDecision_DirectUpdate checks the rule a push meets: a branch a
// land alone may update refuses one, and one that allows them accepts it.
func TestGetBranchDecision_DirectUpdate(t *testing.T) {
	t.Parallel()

	h := newHarness(t,
		config.GraftBranchRule{Name: "main"},
		config.GraftBranchRule{Name: "release", AllowDirectUpdate: true},
	)
	main := h.write("", "main", map[string]string{"a.txt": "a\n"})
	release := h.write("", "release", map[string]string{"b.txt": "b\n"})

	// A push to a branch that only a land may update is refused.
	resp, err := h.policy.GetBranchDecision(h.ctx, &gitalypb.GetBranchDecisionRequest{
		Repository: h.repo,
		Branch:     "main",
		Commits:    []string{main.String()},
		Direct:     true,
	})
	require.NoError(t, err)
	requireDecision(t, resp, "direct_update_denied")

	// The same update of a branch whose rule allows it is accepted.
	resp, err = h.policy.GetBranchDecision(h.ctx, &gitalypb.GetBranchDecisionRequest{
		Repository: h.repo,
		Branch:     "release",
		Commits:    []string{release.String()},
		Direct:     true,
	})
	require.NoError(t, err)
	requireDecision(t, resp, "allowed")
	require.Equal(t, "release", resp.GetBranch())
}

// TestGetBranchDecision_Missing checks the answers about a branch that is not
// there, and the request a server refuses.
func TestGetBranchDecision_Missing(t *testing.T) {
	t.Parallel()

	h := newHarness(t,
		config.GraftBranchRule{Name: "main", RequireReview: true},
		// A managed branch that was never created.
		config.GraftBranchRule{Name: "gone", RequireReview: true},
	)
	h.write("", "main", map[string]string{"a.txt": "a\n"})

	// A managed branch that does not exist: there is nothing to land on.
	resp := h.decide("gone")
	requireDecision(t, resp, "no_branch")
	require.Empty(t, resp.GetTip())

	for _, tc := range []struct {
		desc   string
		branch string
	}{
		{desc: "without a branch", branch: ""},
		{desc: "with a reference", branch: "refs/graft/green/main"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := h.policy.GetBranchDecision(h.ctx, &gitalypb.GetBranchDecisionRequest{
				Repository: h.repo,
				Branch:     tc.branch,
			})
			require.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
		})
	}
}

// TestGetBranchDecision_VerifiedResult checks the rule of a merge queue over
// the API: the checkpoint must cover the commit the land produces, so a green
// tip does not stand in for the result of the update, and a caller that does
// not name the result is told so.
func TestGetBranchDecision_VerifiedResult(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftBranchRule{Name: "main", RequireVerifiedResult: true})
	tip := h.write("", "main", map[string]string{"a.txt": "a\n"})

	// The branch is green at its tip: that is not a statement about the result
	// of a land on top of it.
	h.checkpoint("main", tip)
	change := h.write(tip, "", map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	result := h.write(tip, "", map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"})

	resp := h.decideAbout("main", change.String(), result.String())
	requireDecision(t, resp, "not_verified")
	require.Contains(t, resp.GetReason(), tip.String())
	require.Contains(t, resp.GetReason(), result.String())

	// A caller that does not name the result cannot be answered.
	requireDecision(t, h.decide("main", change.String()), "result_unknown")

	// The checkpoint covers the commit the land produces.
	h.checkpoint("main", result)
	resp = h.decideAbout("main", change.String(), result.String())
	requireDecision(t, resp, "allowed")
	require.Equal(t, result.String(), resp.GetCheckpoint())

	// The branch moved after the result was computed: the land writes another
	// commit, and no checkpoint covers it. The answer says the result is
	// stale, so that a queue computes it again instead of publishing a
	// checkpoint for a commit the branch will not hold.
	h.write(tip, "main", map[string]string{"d.txt": "d\n"})
	resp = h.decideAbout("main", change.String(), result.String())
	requireDecision(t, resp, "stale_result")
	require.Contains(t, resp.GetReason(), result.String())
	require.Contains(t, resp.GetReason(), h.tip("main").String())
}

func TestListBranchRules(t *testing.T) {
	t.Parallel()

	h := newHarness(t,
		config.GraftBranchRule{Name: "main", RequireReview: true, RequireApprovals: 2, RequireVerified: true, RequireVerifiedResult: true},
		config.GraftBranchRule{Name: "release/*", AllowDirectUpdate: true, AllowStaleBase: true},
	)

	resp, err := h.policy.ListBranchRules(h.ctx, &gitalypb.ListBranchRulesRequest{Repository: h.repo})
	require.NoError(t, err)
	require.Equal(t, []*gitalypb.BranchRule{
		{
			Name:                  "main",
			RequireReview:         true,
			RequireApprovals:      2,
			RequireVerified:       true,
			RequireVerifiedResult: true,
		},
		{
			Name:              "release/*",
			AllowDirectUpdate: true,
			AllowStaleBase:    true,
		},
	}, resp.GetRules())
}
