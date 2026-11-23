package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPrintGreenCoversEveryShapeOfTheCheckpoint drives the text of green with
// the results the server can return: a branch without a checkpoint, a
// checkpoint that moved, one that did not, and the tree and suites it covers.
func TestPrintGreenCoversEveryShapeOfTheCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		desc string
		res  greenResult
		want []string
	}{
		{
			desc: "no checkpoint",
			res:  greenResult{Branch: "main"},
			want: []string{"the branch main has no checkpoint"},
		},
		{
			desc: "moved from a previous checkpoint",
			res:  greenResult{Branch: "main", Checkpoint: "bbb", Previous: "aaa", Moved: true, FullTree: true},
			want: []string{"moved from aaa to bbb", "certifies the whole tree of the commit bbb"},
		},
		{
			desc: "moved from nothing",
			res:  greenResult{Branch: "main", Checkpoint: "bbb", Moved: true, FullTree: true},
			want: []string{"is now bbb"},
		},
		{
			desc: "green",
			res:  greenResult{Branch: "main", Checkpoint: "bbb", Green: true, FullTree: true},
			want: []string{"the branch is green"},
		},
		{
			desc: "not green",
			res:  greenResult{Branch: "main", Checkpoint: "bbb", FullTree: true},
			want: []string{"the branch is not green"},
		},
		{
			desc: "certifies only a tree",
			res:  greenResult{Branch: "main", Checkpoint: "bbb", CertifiedTree: "ttt"},
			want: []string{"certifies only the tree ttt of the commit bbb"},
		},
		{
			desc: "certifies no tree",
			res:  greenResult{Branch: "main", Checkpoint: "bbb"},
			want: []string{"certifies no tree of the commit bbb"},
		},
	} {
		var out bytes.Buffer
		require.NoError(t, printGreen(&out, false, tc.res), tc.desc)
		for _, want := range tc.want {
			require.Contains(t, out.String(), want, tc.desc)
		}
	}

	// A suite that ran is named with its commit, and a required suite that has
	// no run is named as such.
	var out bytes.Buffer
	require.NoError(t, printGreen(&out, false, greenResult{
		Branch: "main", Checkpoint: "bbb", FullTree: true,
		Suites: []greenSuite{{Name: "unit", Commit: "bbb"}, {Name: "e2e"}},
	}))
	require.Contains(t, out.String(), "certifies the suite unit on bbb")
	require.Contains(t, out.String(), "certifies no run of the suite e2e")
}

// TestPrintGreenReportsAFailingWriter checks that the JSON output of green
// reports the failure of the writer instead of pretending it printed.
func TestPrintGreenReportsAFailingWriter(t *testing.T) {
	require.Error(t, printGreen(failingWriter{}, true, greenResult{Branch: "main"}))
}
