package branchgate

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestSameRulesComparesEveryField covers the reload decision for rules of the
// same branches with a different policy: the rules are not the same, so the
// reload applies and the next decision is made with the new policy.
func TestSameRulesComparesEveryField(t *testing.T) {
	t.Parallel()

	a := []config.GraftBranchRule{{Name: "main", RequireReview: true}}
	b := []config.GraftBranchRule{{Name: "main", RequireReview: false}}

	require.False(t, sameRules(a, b))
	require.False(t, sameRules(b, a))
	require.True(t, sameRules(a, a))
}
