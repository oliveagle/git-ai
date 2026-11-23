package merge3

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLineEndingOf covers the line ending of every line of a file: the last
// line of a file may have none, and its ending is then the one of the line
// before it.
func TestLineEndingOf(t *testing.T) {
	require.Equal(t, endingUnknown, lineEndingOf(nil, 0))
	require.Equal(t, endingUnknown, lineEndingOf([][]byte{}, 0))

	lf := SplitLines([]byte("a\nb\n"))
	require.Equal(t, endingLF, lineEndingOf(lf, 0))
	require.Equal(t, endingLF, lineEndingOf(lf, 1))

	crlf := SplitLines([]byte("a\r\nb\r\n"))
	require.Equal(t, endingCRLF, lineEndingOf(crlf, 0))
	require.Equal(t, endingCRLF, lineEndingOf(crlf, 1))

	// The last line has no ending of its own: it takes the one of the line
	// before it.
	noEnding := SplitLines([]byte("a\r\nb"))
	require.Equal(t, endingCRLF, lineEndingOf(noEnding, 1))

	// A file of one line without an ending has none to tell.
	alone := SplitLines([]byte("a"))
	require.Equal(t, endingUnknown, lineEndingOf(alone, 0))
}

// TestCRLFNeeded covers the line ending of the markers of a conflicting
// region: they end with CRLF when the sides of the conflict do.
func TestCRLFNeeded(t *testing.T) {
	reg := region{ours: Range{Start: 1}, theirs: Range{Start: 1}}

	lf := &Result{
		base:   SplitLines([]byte("base\n")),
		ours:   SplitLines([]byte("ours\n")),
		theirs: SplitLines([]byte("theirs\n")),
	}
	require.False(t, lf.crlfNeeded(reg))

	crlf := &Result{
		base:   SplitLines([]byte("base\r\n")),
		ours:   SplitLines([]byte("ours\r\n")),
		theirs: SplitLines([]byte("theirs\r\n")),
	}
	require.True(t, crlf.crlfNeeded(reg))

	// The base cannot tell, so the markers end with LF.
	mixed := &Result{
		base:   SplitLines([]byte("base\n")),
		ours:   SplitLines([]byte("ours\r\n")),
		theirs: SplitLines([]byte("theirs\r\n")),
	}
	require.False(t, mixed.crlfNeeded(reg))
}

// TestLineDiffOfTwoEmptySides covers the diff of two texts that hold the same
// lines: there is no edit at all.
func TestLineDiffOfTwoEmptySides(t *testing.T) {
	edits, ok := lineDiff(nil, nil)
	require.True(t, ok)
	require.Empty(t, edits)

	edits, ok = lineDiff(SplitLines([]byte("a\n")), SplitLines([]byte("a\n")))
	require.True(t, ok)
	require.Empty(t, edits)
}
