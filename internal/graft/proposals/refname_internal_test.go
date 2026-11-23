package proposals

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
)

// TestRefNameValidRefusesNonHex covers the shape of a proposal reference: the
// directory and the commit must be hex, so a name that spells either with
// anything else is refused before git runs.
func TestRefNameValidRefusesNonHex(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("0", 40)
	require.True(t, RefNameValid(git.ReferenceName("refs/graft/proposals/0123456789abcdef/"+commit)))

	require.False(t, RefNameValid(git.ReferenceName("refs/graft/proposals/0123456789abcdeZ/"+commit)))
	require.False(t, RefNameValid(git.ReferenceName("refs/graft/proposals/0123456789abcdef/"+commit[:39]+"Z")))
}
