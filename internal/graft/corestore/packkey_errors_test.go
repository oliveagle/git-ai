package corestore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParsePackKeyRefusesANonHexSum covers the pack key whose shape is right
// and whose checksum is not: a key is only a key when the checksum decodes, so
// a row that spells it with anything other than hex names no pack.
func TestParsePackKeyRefusesANonHexSum(t *testing.T) {
	t.Parallel()

	key := "repos/42/packs/" + strings.Repeat("z", 64) + ".pack"
	_, _, ok := ParsePackKey(key)
	require.False(t, ok)
	require.False(t, IsOwnedPackKey(key))
}

// TestIsChangelogRefCoversTags covers the tag half of the changelog refs: a
// tag update is recorded like a branch update, and anything else is not.
func TestIsChangelogRefCoversTags(t *testing.T) {
	t.Parallel()

	require.True(t, IsChangelogRef("refs/tags/v1.0.0"))
	require.True(t, IsChangelogRef("refs/heads/main"))
	require.False(t, IsChangelogRef("refs/keep-around/review/0123"))
}
