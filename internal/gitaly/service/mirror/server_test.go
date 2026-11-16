package mirror

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/gitaly/service/repository"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// harness runs a MirrorService against a repository.
type harness struct {
	t      testing.TB
	ctx    context.Context
	cfg    config.Cfg
	repo   *gitalypb.Repository
	mirror gitalypb.MirrorServiceClient
}

// newHarness starts a server that holds the mirror targets of a repository,
// and a repository to declare them for.
func newHarness(t *testing.T, policy config.GraftMirror) *harness {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Mirror = policy
	h := &harness{t: t, ctx: ctx}

	cfg.SocketPath = testserver.RunGitalyServer(t, cfg, func(srv *grpc.Server, deps *service.Dependencies) {
		gitalypb.RegisterMirrorServiceServer(srv, NewServer(deps))
		// gittest.CreateRepository goes through the RepositoryService.
		gitalypb.RegisterRepositoryServiceServer(srv, repository.NewServer(deps))
	})
	h.cfg = cfg

	conn, err := client.New(ctx, cfg.SocketPath)
	require.NoError(t, err)
	t.Cleanup(func() { testhelper.MustClose(t, conn) })
	h.mirror = gitalypb.NewMirrorServiceClient(conn)

	h.repo, _ = gittest.CreateRepository(t, ctx, cfg)
	return h
}

func TestRegisterTarget(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftMirror{})

	resp, err := h.mirror.RegisterTarget(h.ctx, &gitalypb.RegisterMirrorTargetRequest{
		Repository: h.repo,
		Name:       "codeup",
		Url:        "https://example.com/shop.git",
		Branches:   []string{"main", "release/*"},
		Enabled:    true,
	})
	require.NoError(t, err)
	require.Equal(t, "codeup", resp.GetTarget().GetName())
	require.Equal(t, "https://example.com/shop.git", resp.GetTarget().GetUrl())
	require.Equal(t, []string{"main", "release/*"}, resp.GetTarget().GetBranches())
	require.True(t, resp.GetTarget().GetEnabled())

	listed, err := h.mirror.ListTargets(h.ctx, &gitalypb.ListMirrorTargetsRequest{Repository: h.repo})
	require.NoError(t, err)
	require.Len(t, listed.GetTargets(), 1)
	require.Equal(t, "codeup", listed.GetTargets()[0].GetName())
}

// TestRegisterTarget_replaces checks that a target of the same name is
// replaced, so that an operator repairs a URL by declaring it again.
func TestRegisterTarget_replaces(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftMirror{})

	register := func(url string) {
		_, err := h.mirror.RegisterTarget(h.ctx, &gitalypb.RegisterMirrorTargetRequest{
			Repository: h.repo,
			Name:       "codeup",
			Url:        url,
			Branches:   []string{"*"},
			Enabled:    true,
		})
		require.NoError(t, err)
	}
	register("https://example.com/old.git")
	register("https://example.com/new.git")

	listed, err := h.mirror.ListTargets(h.ctx, &gitalypb.ListMirrorTargetsRequest{Repository: h.repo})
	require.NoError(t, err)
	require.Len(t, listed.GetTargets(), 1)
	require.Equal(t, "https://example.com/new.git", listed.GetTargets()[0].GetUrl())
}

func TestRegisterTarget_invalid(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftMirror{})

	for _, tc := range []struct {
		desc   string
		req    *gitalypb.RegisterMirrorTargetRequest
		expect codes.Code
	}{
		{
			desc:   "a target without a name",
			req:    &gitalypb.RegisterMirrorTargetRequest{Url: "https://example.com/shop.git", Branches: []string{"*"}},
			expect: codes.InvalidArgument,
		},
		{
			desc:   "a target without a URL",
			req:    &gitalypb.RegisterMirrorTargetRequest{Name: "codeup", Branches: []string{"*"}},
			expect: codes.InvalidArgument,
		},
		{
			desc:   "a target without a branch pattern",
			req:    &gitalypb.RegisterMirrorTargetRequest{Name: "codeup", Url: "https://example.com/shop.git"},
			expect: codes.InvalidArgument,
		},
		{
			// A repository the RPC cannot resolve is refused before anything is
			// recorded: the target would otherwise hang off a path nothing reads.
			desc:   "a repository that names no storage",
			req:    &gitalypb.RegisterMirrorTargetRequest{Repository: &gitalypb.Repository{RelativePath: "missing.git"}, Name: "codeup", Url: "https://example.com/shop.git", Branches: []string{"*"}},
			expect: codes.InvalidArgument,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			tc.req.Repository = firstNonNil(tc.req.GetRepository(), h.repo)
			_, err := h.mirror.RegisterTarget(h.ctx, tc.req)
			require.Equal(t, tc.expect, status.Code(err), "error: %v", err)
		})
	}
}

// firstNonNil returns repo when the request carries one of its own.
func firstNonNil(repo, fallback *gitalypb.Repository) *gitalypb.Repository {
	if repo.GetRelativePath() != "" {
		return repo
	}
	return fallback
}

// TestListTargets_policy checks that the policy the server decides with is
// the one the command shows, so that graft mirror status explains a refusal.
func TestListTargets_policy(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftMirror{Required: true, WarnLag: 5, DenyLag: 15})

	listed, err := h.mirror.ListTargets(h.ctx, &gitalypb.ListMirrorTargetsRequest{Repository: h.repo})
	require.NoError(t, err)
	require.True(t, listed.GetPolicy().GetRequired())
	require.Equal(t, int32(5), listed.GetPolicy().GetWarnLag())
	require.Equal(t, int32(15), listed.GetPolicy().GetDenyLag())
	require.Empty(t, listed.GetTargets())
}

func TestUnregisterTarget(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftMirror{})

	_, err := h.mirror.RegisterTarget(h.ctx, &gitalypb.RegisterMirrorTargetRequest{
		Repository: h.repo,
		Name:       "codeup",
		Url:        "https://example.com/shop.git",
		Branches:   []string{"*"},
		Enabled:    true,
	})
	require.NoError(t, err)

	_, err = h.mirror.UnregisterTarget(h.ctx, &gitalypb.UnregisterMirrorTargetRequest{Repository: h.repo, Name: "codeup"})
	require.NoError(t, err)

	listed, err := h.mirror.ListTargets(h.ctx, &gitalypb.ListMirrorTargetsRequest{Repository: h.repo})
	require.NoError(t, err)
	require.Empty(t, listed.GetTargets())

	_, err = h.mirror.UnregisterTarget(h.ctx, &gitalypb.UnregisterMirrorTargetRequest{Repository: h.repo, Name: "codeup"})
	require.Equal(t, codes.NotFound, status.Code(err), "error: %v", err)
}

// TestListTargets_neverSynced checks that a target that never published is
// listed without a status, which the policy refuses when it is required.
func TestListTargets_neverSynced(t *testing.T) {
	t.Parallel()

	h := newHarness(t, config.GraftMirror{Required: true})

	_, err := h.mirror.RegisterTarget(h.ctx, &gitalypb.RegisterMirrorTargetRequest{
		Repository: h.repo,
		Name:       "codeup",
		Url:        "https://example.com/shop.git",
		Branches:   []string{"*"},
		Enabled:    true,
	})
	require.NoError(t, err)

	listed, err := h.mirror.ListTargets(h.ctx, &gitalypb.ListMirrorTargetsRequest{Repository: h.repo})
	require.NoError(t, err)
	require.Len(t, listed.GetTargets(), 1)
	require.Empty(t, listed.GetTargets()[0].GetStatus())
	require.Zero(t, listed.GetTargets()[0].GetLag())
}
