package operations

import (
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
)

// TestUserLand_expectedResult checks that a land that names the commit it
// verified writes exactly that commit: the queue lands the tree it tested, and
// a branch that moved in between refuses the land instead of taking a commit
// nobody tested.
func TestUserLand_expectedResult(t *testing.T) {
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

	// The dry run names the commit the land would write.
	resp, err := client.UserLand(ctx, req)
	require.NoError(t, err)
	verified := resp.GetLandedCommitId()
	require.NotEmpty(t, verified)

	// The branch moves: the land would replay the change onto the new tip and
	// write another commit than the one the caller verified.
	moved := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "other", Content: "other"},
		),
	)

	req.DryRun = false
	req.ExpectedResult = verified
	_, err = client.UserLand(ctx, req)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	// The failure names the commit the land would have written, so that the
	// queue knows it has to compute the result again.
	detail := landErrorDetail(t, err).GetExpectedResult()
	require.NotNil(t, detail)
	require.Equal(t, verified, detail.GetExpected())
	require.NotEqual(t, verified, detail.GetLanded())
	require.Equal(t, moved.String(), detail.GetTip())

	// Nothing was written: the branch holds the commit it held before.
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, moved)

	// The queue computes the result again on the tip the branch holds, and
	// lands that one: the branch takes the commit that was verified.
	req.DryRun = true
	resp, err = client.UserLand(ctx, req)
	require.NoError(t, err)
	verified = resp.GetLandedCommitId()
	require.NotEqual(t, moved.String(), verified)

	req.DryRun = false
	req.ExpectedResult = verified
	landed, err := client.UserLand(ctx, req)
	require.NoError(t, err)
	require.Equal(t, verified, landed.GetLandedCommitId())
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, git.ObjectID(verified))
}

// TestUserLand_expectedResultLandsAlone checks that a land that names the
// commit it verified is not folded into a batch: a batch writes one commit for
// all of its members, which is not the commit the caller verified.
func TestUserLand_expectedResultLandsAlone(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupLandBatchService(t,
		featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false),
		batchingGraft(),
	)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}),
	)
	change := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "change", Content: "change"},
		),
	)

	req := landRequest(repoProto, git.DefaultBranch, change)
	req.Timestamp = timestamppb.New(time.Unix(1700000000, 0))
	req.DryRun = true
	resp, err := client.UserLand(ctx, req)
	require.NoError(t, err)
	verified := resp.GetLandedCommitId()

	req.DryRun = false
	req.ExpectedResult = verified
	landed, err := client.UserLand(ctx, req)
	require.NoError(t, err)
	require.Equal(t, verified, landed.GetLandedCommitId())
	require.Equal(t, int32(1), landed.GetBatchSize(), "a land that names its commit lands alone")
	require.Equal(t, int32(1), landed.GetBatchPosition())
}
