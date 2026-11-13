package branchgate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// review returns a review of the change on the branch, based on the tip.
func review(branch, head, base string, verdicts []*gitalypb.ReviewVerdict) *gitalypb.Review {
	return &gitalypb.Review{
		Id:           "1",
		TargetBranch: []byte(branch),
		HeadCommitId: head,
		BaseCommitId: base,
		Author:       "author",
		Files:        []*gitalypb.ReviewFile{{Path: []byte("main.go"), Class: gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED}},
		Verdicts:     verdicts,
		UpdatedAt:    timestamppb.New(time.Unix(1700000000, 0)),
	}
}

// approve returns an approval by the actor.
func approve(actor string) *gitalypb.ReviewVerdict {
	return &gitalypb.ReviewVerdict{
		Actor:   actor,
		Kind:    gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED,
		Current: true,
	}
}

// requestChanges returns a change request by the actor.
func requestChanges(actor string) *gitalypb.ReviewVerdict {
	return &gitalypb.ReviewVerdict{
		Actor:   actor,
		Kind:    gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES,
		Current: true,
	}
}

func TestDecide_unmanagedBranch(t *testing.T) {
	t.Parallel()

	decision := Decide(Input{Branch: "main", Direct: true, Changes: []string{"commit"}})

	require.True(t, decision.Allowed)
	require.Equal(t, CodeNotManaged, decision.Code)
	require.Nil(t, decision.Rule)
}

func TestDecide_directUpdate(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		rule        config.GraftBranchRule
		wantAllowed bool
		wantCode    string
	}{
		{
			desc:        "a managed branch is only updated by a land",
			rule:        config.GraftBranchRule{Name: "main"},
			wantAllowed: false,
			wantCode:    CodeDirectUpdateDenied,
		},
		{
			desc:        "a rule that allows direct updates lets a push through",
			rule:        config.GraftBranchRule{Name: "main", AllowDirectUpdate: true},
			wantAllowed: true,
			wantCode:    CodeAllowed,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			decision := Decide(Input{
				Rules:   []config.GraftBranchRule{tc.rule},
				Branch:  "main",
				Tip:     "tip",
				Changes: []string{"commit"},
				Direct:  true,
			})

			require.Equal(t, tc.wantAllowed, decision.Allowed)
			require.Equal(t, tc.wantCode, decision.Code)
		})
	}
}

func TestDecide_creationAndDeletion(t *testing.T) {
	t.Parallel()

	decision := Decide(Input{
		Rules:    []config.GraftBranchRule{{Name: "main"}},
		Branch:   "main",
		Creation: true,
	})
	require.False(t, decision.Allowed)
	require.Equal(t, CodeCreationDenied, decision.Code)

	decision = Decide(Input{
		Rules:    []config.GraftBranchRule{{Name: "main"}},
		Branch:   "main",
		Tip:      "tip",
		Deletion: true,
	})
	require.False(t, decision.Allowed)
	require.Equal(t, CodeDeletionDenied, decision.Code)
}

func TestDecide_review(t *testing.T) {
	t.Parallel()

	rules := []config.GraftBranchRule{{Name: "main", RequireReview: true}}

	for _, tc := range []struct {
		desc        string
		reviews     []*gitalypb.Review
		wantAllowed bool
		wantCode    string
	}{
		{
			desc:        "a change without a review does not land",
			reviews:     nil,
			wantAllowed: false,
			wantCode:    CodeNoReview,
		},
		{
			desc:        "a change with an approved review lands",
			reviews:     []*gitalypb.Review{review("main", "commit", "tip", []*gitalypb.ReviewVerdict{approve("reviewer")})},
			wantAllowed: true,
			wantCode:    CodeAllowed,
		},
		{
			desc:        "a change whose review was based on an older tip does not land",
			reviews:     []*gitalypb.Review{review("main", "commit", "older", []*gitalypb.ReviewVerdict{approve("reviewer")})},
			wantAllowed: false,
			wantCode:    CodeStaleReview,
		},
		{
			desc:        "a change with a review that asked for changes does not land",
			reviews:     []*gitalypb.Review{review("main", "commit", "tip", []*gitalypb.ReviewVerdict{requestChanges("reviewer")})},
			wantAllowed: false,
			wantCode:    CodeNotApproved,
		},
		{
			desc:        "a change without an approval does not land",
			reviews:     []*gitalypb.Review{review("main", "commit", "tip", nil)},
			wantAllowed: false,
			wantCode:    CodeNotApproved,
		},
		{
			desc:        "a review of another branch does not answer",
			reviews:     []*gitalypb.Review{review("other", "commit", "tip", []*gitalypb.ReviewVerdict{approve("reviewer")})},
			wantAllowed: false,
			wantCode:    CodeNoReview,
		},
		{
			desc:        "a review of another change does not answer",
			reviews:     []*gitalypb.Review{review("main", "other", "tip", []*gitalypb.ReviewVerdict{approve("reviewer")})},
			wantAllowed: false,
			wantCode:    CodeNoReview,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			decision := Decide(Input{
				Rules:       rules,
				ReviewRules: config.DefaultGraftReviewRules,
				Branch:      "main",
				Tip:         "tip",
				Changes:     []string{"commit"},
				Reviews:     tc.reviews,
			})

			require.Equal(t, tc.wantAllowed, decision.Allowed)
			require.Equal(t, tc.wantCode, decision.Code)
		})
	}
}

func TestDecide_staleBase(t *testing.T) {
	t.Parallel()

	decision := Decide(Input{
		Rules:       []config.GraftBranchRule{{Name: "main", RequireReview: true, AllowStaleBase: true}},
		ReviewRules: config.DefaultGraftReviewRules,
		Branch:      "main",
		Tip:         "tip",
		Changes:     []string{"commit"},
		Reviews:     []*gitalypb.Review{review("main", "commit", "older", []*gitalypb.ReviewVerdict{approve("reviewer")})},
	})

	require.True(t, decision.Allowed)
	require.Equal(t, CodeAllowed, decision.Code)
}

func TestDecide_approvals(t *testing.T) {
	t.Parallel()

	rule := config.GraftBranchRule{Name: "main", RequireApprovals: 2}

	decision := Decide(Input{
		Rules:       []config.GraftBranchRule{rule},
		ReviewRules: config.DefaultGraftReviewRules,
		Branch:      "main",
		Tip:         "tip",
		Changes:     []string{"commit"},
		Reviews:     []*gitalypb.Review{review("main", "commit", "tip", []*gitalypb.ReviewVerdict{approve("reviewer")})},
	})
	require.False(t, decision.Allowed)
	require.Equal(t, CodeNotEnoughApprovals, decision.Code)

	decision = Decide(Input{
		Rules:       []config.GraftBranchRule{rule},
		ReviewRules: config.DefaultGraftReviewRules,
		Branch:      "main",
		Tip:         "tip",
		Changes:     []string{"commit"},
		Reviews: []*gitalypb.Review{review("main", "commit", "tip",
			[]*gitalypb.ReviewVerdict{approve("reviewer"), approve("other")})},
	})
	require.True(t, decision.Allowed)
	require.Equal(t, CodeAllowed, decision.Code)
}

func TestDecide_supersededApproval(t *testing.T) {
	t.Parallel()

	// The approval of the reviewer was replaced by a change request, so it does
	// not count any more.
	verdicts := []*gitalypb.ReviewVerdict{
		approve("reviewer"),
		{Kind: gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES, Actor: "reviewer", Current: true},
	}

	decision := Decide(Input{
		Rules:       []config.GraftBranchRule{{Name: "main", RequireApprovals: 1}},
		ReviewRules: config.DefaultGraftReviewRules,
		Branch:      "main",
		Tip:         "tip",
		Changes:     []string{"commit"},
		Reviews:     []*gitalypb.Review{review("main", "commit", "tip", verdicts)},
	})

	require.False(t, decision.Allowed)
}

func TestDecide_verification(t *testing.T) {
	t.Parallel()

	rule := config.GraftBranchRule{Name: "main", RequireVerified: true}

	decision := Decide(Input{
		Rules:    []config.GraftBranchRule{rule},
		Branch:   "main",
		Tip:      "tip",
		Changes:  []string{"commit"},
		Verified: false,
	})
	require.False(t, decision.Allowed)
	require.Equal(t, CodeNotVerified, decision.Code)

	decision = Decide(Input{
		Rules:    []config.GraftBranchRule{rule},
		Branch:   "main",
		Tip:      "tip",
		Changes:  []string{"commit"},
		Verified: true,
	})
	require.True(t, decision.Allowed)
	require.Equal(t, CodeAllowed, decision.Code)
}

func TestDecide_everyChangeOfALand(t *testing.T) {
	t.Parallel()

	decision := Decide(Input{
		Rules:       []config.GraftBranchRule{{Name: "main", RequireReview: true}},
		ReviewRules: config.DefaultGraftReviewRules,
		Branch:      "main",
		Tip:         "tip",
		Changes:     []string{"first", "second"},
		Reviews: []*gitalypb.Review{
			review("main", "first", "tip", []*gitalypb.ReviewVerdict{approve("reviewer")}),
		},
	})

	require.False(t, decision.Allowed)
	require.Equal(t, CodeNoReview, decision.Code)
	require.Equal(t, "second", decision.Change)
}

func TestRule(t *testing.T) {
	t.Parallel()

	rules := []config.GraftBranchRule{{Name: "main", RequireReview: true}, {Name: "release/1.0"}}

	rule := Rule(rules, "main")
	require.NotNil(t, rule)
	require.Equal(t, "main", rule.Name)

	rule = Rule(rules, "other")
	require.Nil(t, rule)
}

func TestDecide_missingBranch(t *testing.T) {
	t.Parallel()

	decision := Decide(Input{
		Rules:   []config.GraftBranchRule{{Name: "main"}},
		Branch:  "main",
		Changes: []string{"commit"},
	})

	require.False(t, decision.Allowed)
	require.Equal(t, CodeNoBranch, decision.Code)
}
