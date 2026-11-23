package policyeval

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestEvaluateUpdateRefusesABrokenPolicy covers the policy whose actor is not
// a glob the matcher can read: the error must reach the caller, so that the
// server refuses to guess.
func TestEvaluateUpdateRefusesABrokenPolicy(t *testing.T) {
	policy := []config.GraftPolicyRule{{Actor: "[", Refs: []string{"refs/**"}, Allow: true}}

	_, _, err := EvaluateUpdate(policy, "alice", "refs/heads/main", "a", "b")
	require.Error(t, err)
	require.Contains(t, err.Error(), "policy actor")

	_, err = Denial(policy, "alice", []byte("a b refs/heads/main\n"))
	require.Error(t, err)
}

// TestDenialIgnoresTheReferencesThePolicyDoesNotGovern covers the change of a
// reference outside refs/ and HEAD: the policy does not govern it.
func TestDenialIgnoresTheReferencesThePolicyDoesNotGovern(t *testing.T) {
	policy := []config.GraftPolicyRule{{Refs: []string{"refs/**"}}}

	message, err := Denial(policy, "alice", []byte("a b FETCH_HEAD\n"))
	require.NoError(t, err)
	require.Empty(t, message)
}

// TestARuleThatDoesNotApplyDeniesTheUpdate covers the update that no rule
// governs: the policy denies it.
func TestARuleThatDoesNotApplyDeniesTheUpdate(t *testing.T) {
	policy := []config.GraftPolicyRule{{Actor: "bob", Refs: []string{"refs/**"}, Allow: true}}

	allowed, message, err := EvaluateUpdate(policy, "alice", "refs/heads/main", "a", "b")
	require.NoError(t, err)
	require.False(t, allowed)
	require.Contains(t, message, "denies")
}

// TestMatchRefRefusesABrokenGlob covers the glob of a policy the matcher
// cannot read.
func TestMatchRefRefusesABrokenGlob(t *testing.T) {
	_, err := matchRef("[/**", "refs/heads/main")
	require.Error(t, err)

	_, err = matchRef("a[**", "refs/heads/main")
	require.Error(t, err)

	_, err = matchRefPrefix("[", "refs/heads/main")
	require.Error(t, err)
}
