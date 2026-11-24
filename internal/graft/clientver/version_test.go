package clientver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParse covers the shapes a version is reported in: a release, a release
// with a "v", a pre-release and the describe suffix of a build between two
// releases. Anything that is not a version is not parsed, because a server
// must not compare a number it guessed.
func TestParse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want Version
	}{
		{"0.9.1", Version{Major: 0, Minor: 9, Patch: 1}},
		{" v0.9.1 ", Version{Major: 0, Minor: 9, Patch: 1}},
		{"1.0.0", Version{Major: 1, Minor: 0, Patch: 0}},
		{"0.10.0-rc.2", Version{Major: 0, Minor: 10, Patch: 0, Pre: "rc.2"}},
		{"0.9.1-2-g1a2b3c4", Version{Major: 0, Minor: 9, Patch: 1, Commits: 2}},
		{"0.9.1-0-g1a2b3c4", Version{Major: 0, Minor: 9, Patch: 1}},
	} {
		got, ok := Parse(tc.in)
		require.True(t, ok, tc.in)
		require.Equal(t, tc.want, got, tc.in)
	}

	for _, bad := range []string{"", "  ", "unknown", "dev", "0.9", "0.9.1.2", "x.y.z", "0.9.x", "01.9.1", "-1.0.0", "0.9.1-"} {
		_, ok := Parse(bad)
		require.False(t, ok, bad)
	}
}

// TestCompare covers the order a gate is decided on: numbers first, then a
// release above its pre-releases, then a build past a release above it.
func TestCompare(t *testing.T) {
	t.Parallel()

	parse := func(s string) Version {
		v, ok := Parse(s)
		require.True(t, ok, s)
		return v
	}

	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.9.1", "0.9.1", 0},
		{"0.9.0", "0.9.1", -1},
		{"0.10.0", "0.9.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.9.1-rc.1", "0.9.1", -1},
		{"0.9.1", "0.9.1-rc.1", 1},
		{"0.9.1-rc.1", "0.9.1-rc.2", -1},
		{"0.9.1-2-gabc", "0.9.1", 1},
		{"0.9.1", "0.9.1-2-gabc", -1},
		{"0.9.1-2-gabc", "0.9.1-3-gabc", -1},
		{"0.9.1-2-gabc", "0.9.2", -1},
	} {
		require.Equal(t, tc.want, parse(tc.a).Compare(parse(tc.b)), "%s vs %s", tc.a, tc.b)
	}
}

// TestString covers the spelling of the minimum in the refusal and in the log:
// the describe suffix says how far past a release a build is, not which
// release it is, so it is not part of the number a server asks for.
func TestString(t *testing.T) {
	t.Parallel()

	v, ok := Parse("0.9.1-2-g1a2b3c4")
	require.True(t, ok)
	require.Equal(t, "0.9.1", v.String())

	rc, ok := Parse("0.10.0-rc.1")
	require.True(t, ok)
	require.Equal(t, "0.10.0-rc.1", rc.String())
}
