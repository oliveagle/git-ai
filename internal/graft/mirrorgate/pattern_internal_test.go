package mirrorgate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMatchBranchPatternRefusesAnEmptyPattern covers the pattern that names
// no branch: it publishes nothing.
func TestMatchBranchPatternRefusesAnEmptyPattern(t *testing.T) {
	require.False(t, matchBranchPattern("", "main"))
	require.True(t, matchBranchPattern("main", "main"))
	require.True(t, matchBranchPattern("*", ""))
	require.True(t, matchBranchPattern("release/*", "release/1.0"))
	require.False(t, matchBranchPattern("release/*", "main"))
}

// TestDecideFollowsTheDivergedMirrorOfAnotherLag covers the mirror that
// diverged while another one is further behind: the decision is about the
// divergence, whichever mirror carries it.
func TestDecideFollowsTheDivergedMirrorOfAnotherLag(t *testing.T) {
	behind := Target{Name: "github", Branches: []string{"main"}, Enabled: true, Synced: true, Lag: 5}
	diverged := Target{Name: "gitee", Branches: []string{"main"}, Enabled: true, Synced: true, Lag: 3, Diverged: true}

	decision := Decide(Input{
		Policy:     Policy{Required: true, WarnLag: 10, DenyLag: 20},
		Repository: "shop.git",
		Branch:     "main",
		Targets:    []Target{behind, diverged},
	})
	require.False(t, decision.Allowed)
	require.Equal(t, CodeDiverged, decision.Code)
}
