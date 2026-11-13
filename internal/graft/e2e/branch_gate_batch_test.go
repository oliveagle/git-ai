package e2e

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/sdk"
)

// TestBranchGateGatesTheLandsOfABatch checks that a batch is not a way around
// the policy: the land lanes publish the combined update in the name of the
// batch, and the changes it folds in are agents' lands, so the gate decides
// them too. Two changes without a review arrive at once; the combined update
// is refused, each change lands alone, and each is refused for its own reason,
// so the branch does not move.
func TestBranchGateGatesTheLandsOfABatch(t *testing.T) {
	t.Parallel()

	s := startWithBranchRule(t, config.GraftBranchRule{Name: "main", RequireReview: true})
	base := writeMain(t, s, map[string]string{"README.md": "main\n"})

	builder := s.repoAs(t, exampleTokens["builder"])
	lands := make([]sdk.LandOptions, 0, 2)
	for i, name := range []string{"a.txt", "b.txt"} {
		commit, err := commitFile(s.ctx, builder, "builder", "feature-"+name, base, name, name+"\n")
		require.NoError(t, err)
		lands = append(lands, sdk.LandOptions{
			Branch:    "main",
			Commit:    commit,
			User:      sdk.User{Name: clientDisplayName, Email: "builder@example.com"},
			Timestamp: time.Unix(int64(1700001000+100*i), 0),
		})
	}

	errs := make([]error, len(lands))
	var wg sync.WaitGroup
	for i := range lands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = builder.Land(s.ctx, lands[i])
		}(i)
	}
	wg.Wait()

	for i := range errs {
		require.Error(t, errs[i], "a change without a review does not land, also not in a batch")
		require.Contains(t, errs[i].Error(), "has no review")
	}
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"), "the refused lands moved nothing")
}
