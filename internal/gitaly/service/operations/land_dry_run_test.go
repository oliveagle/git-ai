package operations

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestUserLand_dryRun checks that a dry run names the commit a land would
// produce, writes nothing of the branch, and that the land with the same
// request produces exactly that commit: it is the loop a merge queue runs
// (verify the result, publish it as the checkpoint, land it).
func TestUserLand_dryRun(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx, cfg, client := setupOperationsServiceWithCfg(t,
		featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false),
		cfg,
	)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}),
	)
	// The branch moves on, so the change has to be replayed onto the new tip and
	// the commit a land produces is not the commit the caller submits.
	gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "tip", Content: "tip"},
		),
	)
	change := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "change", Content: "change"},
		),
	)

	timestamp := time.Unix(1700000000, 0)
	req := landRequest(repoProto, git.DefaultBranch, change)
	req.Timestamp = timestamppb.New(timestamp)
	req.DryRun = true

	tipBefore := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch)

	resp, err := client.UserLand(ctx, req)
	require.NoError(t, err)
	require.True(t, resp.GetDryRun(), "a dry run says that it is one")
	require.NotEmpty(t, resp.GetLandedCommitId())
	require.NotEqual(t, change.String(), resp.GetLandedCommitId(), "the branch moved, so the land rewrites the change")

	// Nothing of the branch changed.
	require.Equal(t, tipBefore.String(), gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch).String())

	// The commit a dry run names is readable: a CI checks it out and tests it.
	produced := resp.GetLandedCommitId()
	require.Equal(t, tipBefore.String(), strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", produced+"^"))))
	require.Equal(t, "change", strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "-p", produced+":change"))))

	// The land of the same request produces exactly that commit.
	req.DryRun = false
	landed, err := client.UserLand(ctx, req)
	require.NoError(t, err)
	require.False(t, landed.GetDryRun())
	require.Equal(t, produced, landed.GetLandedCommitId())
	require.Equal(t, produced, gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch).String())
}

// TestUserLand_dryRunRefusesResolution checks that a dry run refuses the two
// requests whose result it cannot name: a conflict the server resolves and a
// resolution the caller supplies.
func TestUserLand_dryRunRefusesResolution(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t)
	ctx, cfg, client := setupOperationsServiceWithCfg(t,
		featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false),
		cfg,
	)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base"}),
	)
	change := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "b", Content: "change"}),
	)

	for _, tc := range []struct {
		desc string
		req  *gitalypb.UserLandRequest
	}{
		{
			desc: "resolved by the server",
			req: func() *gitalypb.UserLandRequest {
				req := landRequest(repoProto, git.DefaultBranch, change)
				req.DryRun = true
				req.ResolveConflicts = true
				return req
			}(),
		},
		{
			desc: "resolved by the caller",
			req: func() *gitalypb.UserLandRequest {
				req := landRequest(repoProto, git.DefaultBranch, change)
				req.DryRun = true
				req.Resolution = &gitalypb.LandResolution{Tip: base.String(), Commit: change.String()}
				return req
			}(),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := client.UserLand(ctx, tc.req)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
}
