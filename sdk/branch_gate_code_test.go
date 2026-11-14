package sdk_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// TestBranchGateCode checks that a refusal of the branch management policy
// carries its code, so that a CI branches on the code instead of parsing the
// reason of the message.
func TestBranchGateCode(t *testing.T) {
	t.Parallel()

	st, err := status.New(codes.PermissionDenied, "the branch \"main\" has no verification checkpoint").WithDetails(&gitalypb.BranchGateError{
		Branch: "main",
		Code:   "not_verified",
		Reason: "the branch \"main\" has no verification checkpoint",
	})
	require.NoError(t, err)

	denial, ok := sdk.BranchGateDenial(st.Err())
	require.True(t, ok)
	require.Equal(t, "main", denial.GetBranch())
	require.Equal(t, "not_verified", denial.GetCode())
	require.Equal(t, "not_verified", sdk.BranchGateCode(st.Err()))

	// An error that is not a refusal of the policy has no code.
	require.Equal(t, "", sdk.BranchGateCode(errors.New("the server is down")))
	require.Equal(t, "", sdk.BranchGateCode(status.Error(codes.Internal, "the server is down")))
}
