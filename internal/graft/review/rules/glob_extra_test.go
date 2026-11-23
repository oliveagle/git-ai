package rules

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMatchGlobRefusesAMalformedPattern covers the glob the matcher cannot
// read: the error must reach the caller, so that a policy with a broken glob
// is refused instead of matching nothing.
func TestMatchGlobRefusesAMalformedPattern(t *testing.T) {
	_, err := matchGlob("[/**", "a/b")
	require.Error(t, err)

	_, err = matchGlob("a[**", "ab")
	require.Error(t, err)
}
