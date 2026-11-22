package greenref

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNamesOfTheCheckpointAndItsTree checks that the two references of a
// branch stay in their own namespaces, so that the checkpoint of a branch and
// the tree it certifies never share a name.
func TestNamesOfTheCheckpointAndItsTree(t *testing.T) {
	require.Equal(t, "refs/graft/green/main", Name("main"))
	require.Equal(t, "refs/graft/green/main", Name("refs/heads/main"))
	require.Equal(t, "refs/graft/green-tree/main", TreeName("main"))
	require.Equal(t, "refs/graft/green-tree/main", TreeName("refs/heads/main"))

	// The tree of a checkpoint is the tree of the same branch, and the two
	// names are never the same.
	require.Equal(t, "refs/graft/green-tree/main", TreeNameFor("refs/graft/green/main"))
	require.NotEqual(t, Name("main"), TreeName("main"))
}
