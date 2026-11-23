package merge3

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMergeTooLargeOnTheTheirsSide covers the second diff: a base and an ours
// that agree cost nothing, while theirs rewrites every line, so the table that
// exceeds MaxCells is theirs and the merge fails after the first side has
// already been diffed.
func TestMergeTooLargeOnTheTheirsSide(t *testing.T) {
	t.Parallel()

	var a, b strings.Builder
	for i := 0; i < 3000; i++ {
		a.WriteString("a\n")
		b.WriteString("b\n")
	}
	_, ok := Merge([]byte(a.String()), []byte(a.String()), []byte(b.String()))
	require.False(t, ok)
}

// TestCRLFNeededWithALFTheirs covers their side of the marker line ending:
// ours ends with CRLF and theirs with LF, so the markers keep LF — ours alone
// does not decide.
func TestCRLFNeededWithALFTheirs(t *testing.T) {
	t.Parallel()

	reg := region{ours: Range{Start: 1}, theirs: Range{Start: 1}}
	r := &Result{
		base:   SplitLines([]byte("base\r\n")),
		ours:   SplitLines([]byte("ours\r\n")),
		theirs: SplitLines([]byte("theirs\n")),
	}
	require.False(t, r.crlfNeeded(reg))
}
