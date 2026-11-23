package greenref

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidSuiteName covers the names a suite may carry: a reference path
// component without a slash, and none of the characters Git refuses.
func TestValidSuiteName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"unit", "e2e", "build-1", "a.b", "ünïcode", `quote"`} {
		require.True(t, ValidSuiteName(name), name)
	}

	for _, name := range []string{
		"",
		".hidden",
		"unit.lock",
		"with space",
		"tilde~1",
		"caret^",
		"colon:",
		"question?",
		"star*",
		"bracket[",
		"backslash\\",
		"slash/name",
		"control\x01",
		"delete\x7f",
	} {
		require.False(t, ValidSuiteName(name), name)
	}
}

// TestSuitesNameHoldsTheSuiteAndTheBranch covers the reference that holds the
// commit a suite certified: the suite is the first component and the branch is
// the rest, so a branch with a slash in it stays one branch.
func TestSuitesNameHoldsTheSuiteAndTheBranch(t *testing.T) {
	t.Parallel()

	require.Equal(t, "refs/graft/green-suite/unit/main", SuitesName("unit", "main"))
	require.Equal(t, "refs/graft/green-suite/unit/main", SuitesName("unit", "refs/heads/main"))
	require.Equal(t, "refs/graft/green-suite/unit/feature/x", SuitesName("unit", "feature/x"))
	require.Equal(t, "refs/graft/green-suite/unit/feature", SuitesName("unit", "refs/heads/feature"))
}
