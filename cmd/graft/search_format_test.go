package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/sdk"
)

// TestPathListPrintsThePaths covers the string of a repeatable path flag.
func TestPathListPrintsThePaths(t *testing.T) {
	t.Parallel()

	var p pathList
	require.Equal(t, "", p.String())

	p = pathList{"a", "b"}
	require.Equal(t, "a,b", p.String())
}

// TestWriteSearchTextPrintsTheMatches covers the match output of the search:
// one line per match, context lines, the separator between groups, and no
// context line that was printed already.
func TestWriteSearchTextPrintsTheMatches(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	matches := []sdk.SearchMatch{
		{Path: "a.txt", Line: 2, Text: "hit", Before: []string{"before"}, After: []string{"after"}},
		{Path: "a.txt", Line: 5, Text: "hit"},
		{Path: "b.txt", Line: 1, Text: "hit"},
	}
	require.NoError(t, writeSearchText(&out, matches, true))
	text := out.String()

	require.Contains(t, text, "a.txt-1-before\n")
	require.Contains(t, text, "a.txt:2:hit\n")
	require.Contains(t, text, "a.txt-3-after\n")
	require.Contains(t, text, "--\n")
	require.Contains(t, text, "a.txt:5:hit\n")
	require.Contains(t, text, "b.txt:1:hit\n")

	// Without context the separator is gone but the context lines stay.
	out.Reset()
	require.NoError(t, writeSearchText(&out, matches, false))
	require.NotContains(t, out.String(), "--")

	// A match that follows the last one on the same path without a gap prints
	// no separator.
	out.Reset()
	require.NoError(t, writeSearchText(&out, []sdk.SearchMatch{{Path: "a", Line: 1, Text: "x"}, {Path: "a", Line: 2, Text: "y"}}, true))
	require.NotContains(t, out.String(), "--")
}

// TestWriteSearchTextReportsTheFailuresOfTheOutput covers the output that
// cannot be written.
func TestWriteSearchTextReportsTheFailuresOfTheOutput(t *testing.T) {
	t.Parallel()

	err := writeSearchText(failingWriter{}, []sdk.SearchMatch{{Path: "a", Line: 1, Text: "x"}}, false)
	require.Error(t, err)
}

// TestWriteSearchJSONPrintsTheMatches covers the JSON of the search.
func TestWriteSearchJSONPrintsTheMatches(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	matches := []sdk.SearchMatch{
		{Path: "a.txt", Line: 2, Text: "hit", Before: []string{"before"}, After: []string{"after"}},
		{Path: "b\xff.txt", Line: 1, Text: "hit\xff\xfe"},
	}
	require.NoError(t, writeSearchJSON(&out, matches))
	require.Contains(t, out.String(), `"path":"a.txt"`)
	require.Contains(t, out.String(), `"path_b64":"Yv8udHh0"`)
	require.Contains(t, out.String(), `"text_b64":"aGl0//4="`)

	err := writeSearchJSON(failingWriter{}, matches)
	require.Error(t, err)
}

// failingWriter fails on the first write.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errBackend
}
