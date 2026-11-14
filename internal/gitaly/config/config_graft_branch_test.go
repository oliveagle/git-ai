package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGraft_validateBranches(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc      string
		branches  []GraftBranchRule
		expectErr string
	}{
		{
			desc:     "no rule at all is valid",
			branches: nil,
		},
		{
			desc:     "a rule of a branch is valid",
			branches: []GraftBranchRule{{Name: "main", RequireReview: true}},
		},
		{
			desc:     "a rule of a branch with a slash is valid",
			branches: []GraftBranchRule{{Name: "release/1.0"}},
		},
		{
			desc:      "a rule without a name is refused",
			branches:  []GraftBranchRule{{RequireReview: true}},
			expectErr: "name is required",
		},
		{
			desc:      "a rule of a reference is refused",
			branches:  []GraftBranchRule{{Name: "refs/heads/main"}},
			expectErr: "is a reference name",
		},
		{
			desc:      "a rule of an invalid branch name is refused",
			branches:  []GraftBranchRule{{Name: "main..other"}},
			expectErr: "is not a valid branch name",
		},
		{
			desc:      "a rule of a branch that ends with a dot is refused",
			branches:  []GraftBranchRule{{Name: "main."}},
			expectErr: "is not a valid branch name",
		},
		{
			desc:      "a rule with a negative number of approvals is refused",
			branches:  []GraftBranchRule{{Name: "main", RequireApprovals: -1}},
			expectErr: "require_approvals must not be negative",
		},
		{
			desc:     "a rule that requires suites is valid",
			branches: []GraftBranchRule{{Name: "main", RequireSuites: []string{"unit", "integration"}}},
		},
		{
			desc:      "a suite with a slash is refused",
			branches:  []GraftBranchRule{{Name: "main", RequireSuites: []string{"unit/one"}}},
			expectErr: "is not a valid suite name",
		},
		{
			desc:      "an empty suite is refused",
			branches:  []GraftBranchRule{{Name: "main", RequireSuites: []string{""}}},
			expectErr: "is not a valid suite name",
		},
		{
			desc:      "the same suite twice is refused",
			branches:  []GraftBranchRule{{Name: "main", RequireSuites: []string{"unit", "unit"}}},
			expectErr: "is required twice",
		},
		{
			desc:      "two rules of one branch are refused",
			branches:  []GraftBranchRule{{Name: "main"}, {Name: "main", AllowDirectUpdate: true}},
			expectErr: "has more than one rule",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			err := Graft{Branches: tc.branches}.Validate()
			if tc.expectErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.expectErr)
		})
	}
}

func TestGraft_Branches_areReadFromTheConfiguration(t *testing.T) {
	t.Parallel()

	cfg := `
[graft]
standalone = true

[[graft.branch]]
name = "main"
require_review = true
require_approvals = 2

[[graft.branch]]
name = "release/1.0"
allow_direct_update = true
require_verified = true
`

	loaded, err := Load(strings.NewReader(cfg))
	require.NoError(t, err)
	require.NoError(t, loaded.Graft.Validate())

	require.Len(t, loaded.Graft.Branches, 2)
	require.Equal(t, "main", loaded.Graft.Branches[0].Name)
	require.True(t, loaded.Graft.Branches[0].RequireReview)
	require.EqualValues(t, 2, loaded.Graft.Branches[0].RequireApprovals)
	require.Equal(t, "release/1.0", loaded.Graft.Branches[1].Name)
	require.True(t, loaded.Graft.Branches[1].AllowDirectUpdate)
	require.True(t, loaded.Graft.Branches[1].RequireVerified)
}
