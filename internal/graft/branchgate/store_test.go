package branchgate

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestStoreReload checks what a reload does to the rules in force: a valid new
// rule is what the next decision is made with, the same rules change nothing,
// and a rule the server cannot read leaves the rules in force, so that a typo
// never opens a managed branch.
func TestStoreReload(t *testing.T) {
	t.Parallel()

	graft := config.Graft{Branches: []config.GraftBranchRule{{Name: "main", RequireReview: true}}}
	store := NewStore(graft)
	require.Equal(t, uint64(1), store.Generation())
	require.Equal(t, graft.Branches, store.Current().Rules)

	// The rule in force is what the store hands out, and it is the policy of the
	// configuration: no rule for a branch it does not name.
	require.NotNil(t, store.Current().Rule("main"))
	require.Nil(t, store.Current().Rule("release/1"))

	// A file whose rules the server cannot read is rejected, and the rules in
	// force stay.
	before := store.Current()
	invalid := config.Graft{Branches: []config.GraftBranchRule{{Name: "main"}, {Name: "main"}}}
	_, err := store.Reload(invalid)
	require.ErrorContains(t, err, "more than one rule")
	require.Same(t, before, store.Current())
	require.Equal(t, uint64(1), store.Generation())

	// The same rules change nothing.
	outcome, err := store.Reload(graft)
	require.NoError(t, err)
	require.False(t, outcome.Applied)
	require.Equal(t, uint64(1), outcome.Generation)
	require.Same(t, before, store.Current())

	// New rules are put in force, as the next generation.
	rules := []config.GraftBranchRule{
		{Name: "main", RequireVerified: true, RequireApprovals: 1},
		{Name: "release/1", AllowDirectUpdate: true},
	}
	outcome, err = store.Reload(config.Graft{Branches: rules})
	require.NoError(t, err)
	require.True(t, outcome.Applied)
	require.Equal(t, uint64(2), outcome.Generation)
	require.Equal(t, uint64(2), store.Generation())
	require.Equal(t, rules, store.Current().Rules)
	require.False(t, store.Current().Rule("main").RequireReview, "the old rule is gone with the old policy")

	// The approval rules of [graft.review] are not reloaded: the store keeps the
	// ones in force, as the configuration it started with gave them.
	require.Equal(t, graft.Review.RulesOrDefault(), store.Current().ReviewRules)
}

// TestStoreIsACollector checks that the store exports the reload counters the
// server registers, so that a reload can be watched without reading the log.
func TestStoreIsACollector(t *testing.T) {
	t.Parallel()

	var _ prometheus.Collector = NewStore(config.Graft{})
}
