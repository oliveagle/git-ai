package branchgate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestThePolicyOfNoBranch covers the policy that governs no branch at all.
func TestThePolicyOfNoBranch(t *testing.T) {
	var none *Policy
	require.Nil(t, none.Rule("main"))
	require.True(t, none.Empty())

	empty := &Policy{}
	require.Nil(t, empty.Rule("main"))
	require.True(t, empty.Empty())

	governed := &Policy{Rules: []config.GraftBranchRule{{Name: "main"}}}
	require.False(t, governed.Empty())
	require.NotNil(t, governed.Rule("main"))
}

// TestDescribeSuites covers the reason of a refusal that names no suite.
func TestDescribeSuites(t *testing.T) {
	require.Equal(t, "no suite at all", describeSuites(Input{}))
}

// TestNewer covers the order of two reviews: the update time decides, and the
// identifier breaks a tie.
func TestNewer(t *testing.T) {
	older := &gitalypb.Review{Id: "1", UpdatedAt: timestamppb.New(time.Now())}
	later := &gitalypb.Review{Id: "2", UpdatedAt: timestamppb.New(time.Now().Add(time.Second))}

	require.True(t, newer(later, older))
	require.False(t, newer(older, later))

	// A review without a time is older than one with a time.
	without := &gitalypb.Review{Id: "3"}
	require.True(t, newer(older, without))
	require.False(t, newer(without, older))
	require.False(t, newer(without, without))
}

// TestDescribeRequirements covers the reason of a denial that names the
// requirements that are not satisfied.
func TestDescribeRequirements(t *testing.T) {
	require.Equal(t, "a reviewer requested changes", describeRequirements(nil))
	require.Equal(t, "a reviewer requested changes", describeRequirements([]*gitalypb.ReviewRequirement{
		{Rule: "review", RequiredApprovals: 2, Approvals: 2, Satisfied: true},
	}))

	reason := describeRequirements([]*gitalypb.ReviewRequirement{
		{Rule: "review", RequiredApprovals: 2, Approvals: 1},
	})
	require.Contains(t, reason, "review needs 2 approvals and has 1")
}

// TestAllowNamesTheReason covers the reason of an allowed update.
func TestAllowNamesTheReason(t *testing.T) {
	rule := &config.GraftBranchRule{Name: "main"}
	decision := allow(rule, "abc", "the commit %s is verified", "abc")
	require.True(t, decision.Allowed)
	require.Equal(t, "abc", decision.Change)
	require.Equal(t, "the commit abc is verified", decision.Reason)

	decision = allow(rule, "abc", "")
	require.Empty(t, decision.Reason)
}
