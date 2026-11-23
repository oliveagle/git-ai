package sdk

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestLandConflictFindsThePackageOfAFailure covers the conflict package a
// land carries, and the failures that carry none.
func TestLandConflictFindsThePackageOfAFailure(t *testing.T) {
	t.Parallel()

	conflict := &gitalypb.LandConflict{Tip: "a", Commit: "b"}
	st, err := status.New(codes.FailedPrecondition, "conflicts").WithDetails(conflict)
	require.NoError(t, err)

	pkg, ok := LandConflict(st.Err())
	require.True(t, ok)
	require.Equal(t, conflict.GetTip(), pkg.GetTip())
	require.Equal(t, conflict.GetCommit(), pkg.GetCommit())

	for _, err := range []error{
		nil,
		status.Error(codes.Internal, "no details"),
		errors.New("not a status"),
		statusErrWithOtherDetail(),
	} {
		pkg, ok := LandConflict(err)
		require.False(t, ok, err)
		require.Nil(t, pkg, err)
	}
}

// TestResolutionForNamesThePackage covers the empty resolution of a conflict
// package.
func TestResolutionForNamesThePackage(t *testing.T) {
	t.Parallel()

	c := &gitalypb.LandConflict{Tip: "tip", Commit: "commit"}
	res := ResolutionFor(c)
	require.Equal(t, &Resolution{Tip: "tip", Commit: "commit"}, res)
}

// TestResolutionFromProposalsTakesEveryReadableProposal covers the resolution
// that takes the proposals of the sides as they are: a delete, a write, and a
// path without a readable proposal is left out.
func TestResolutionFromProposalsTakesEveryReadableProposal(t *testing.T) {
	t.Parallel()

	c := &gitalypb.LandConflict{
		Tip:    "tip",
		Commit: "commit",
		Files: []*gitalypb.LandConflictFile{
			{Path: []byte("delete.txt")},
			{Path: []byte("write.txt")},
			{Path: []byte("unreadable.txt")},
		},
	}
	sides := map[string]ConflictSides{
		"delete.txt":   {Proposal: &Proposal{Delete: true}},
		"write.txt":    {Proposal: &Proposal{Mode: "100644", Content: []byte("resolved")}},
		"unreadable.txt": {},
	}

	res := ResolutionFromProposals(c, sides)
	require.Equal(t, "tip", res.Tip)
	require.Equal(t, "commit", res.Commit)
	require.ElementsMatch(t, []ResolvedFile{
		{Path: "delete.txt", Delete: true},
		{Path: "write.txt", Mode: "100644", Content: []byte("resolved")},
	}, res.Files)
}

// TestResolutionToProto covers the request of a resolution: a write carries
// the mode and the content, and a delete carries neither.
func TestResolutionToProto(t *testing.T) {
	t.Parallel()

	res := &Resolution{
		Tip:    "tip",
		Commit: "commit",
		Files: []ResolvedFile{
			{Path: "keep.txt", Mode: "100644", Content: []byte("kept")},
			{Path: "gone.txt", Delete: true},
		},
	}
	proto := res.toProto()

	require.Equal(t, "tip", proto.GetTip())
	require.Equal(t, "commit", proto.GetCommit())
	require.Len(t, proto.GetFiles(), 2)

	require.Equal(t, gitalypb.LandResolvedFile_ACTION_WRITE, proto.GetFiles()[0].GetAction())
	require.Equal(t, "100644", proto.GetFiles()[0].GetMode())
	require.Equal(t, []byte("kept"), proto.GetFiles()[0].GetContent())

	require.Equal(t, gitalypb.LandResolvedFile_ACTION_DELETE, proto.GetFiles()[1].GetAction())
	require.Empty(t, proto.GetFiles()[1].GetMode())
	require.Nil(t, proto.GetFiles()[1].GetContent())
}

// statusErrWithOtherDetail is a status whose detail is not a conflict
// package.
func statusErrWithOtherDetail() error {
	st, err := status.New(codes.FailedPrecondition, "other").WithDetails(&errdetails.DebugInfo{Detail: "x"})
	if err != nil {
		panic(err)
	}
	return st.Err()
}
