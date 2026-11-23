package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// errWriter is a writer that always fails. The commands that print a report
// must report the failure of the output, not swallow it.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("the output failed") }

// TestMirrorStatusPrint covers every state the status of the mirrors can be
// in: a target that never published, a disabled target, a target that is
// behind, a target that diverged and a target whose last publication failed.
func TestMirrorStatusPrint(t *testing.T) {
	for _, tc := range []struct {
		desc   string
		status mirrorStatus
		want   []string
	}{
		{
			desc: "a target that never published",
			status: mirrorStatus{
				Repository: "shop.git",
				Policy:     mirrorStatusPolicy{Required: true},
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Enabled: true}},
				Allowed:    true,
			},
			want: []string{"never published", "the mirrors are up to date"},
		},
		{
			desc: "a disabled target",
			status: mirrorStatus{
				Repository: "shop.git",
				Policy:     mirrorStatusPolicy{Required: true},
				Targets:    []mirrorStatusTarget{{Name: "gitee", URL: "https://gitee.com/g/s.git", Enabled: false, Status: "ok"}},
				Allowed:    true,
			},
			want: []string{", disabled"},
		},
		{
			desc: "a target that is behind and diverged",
			status: mirrorStatus{
				Repository: "shop.git",
				Policy:     mirrorStatusPolicy{Required: true, WarnLag: 10, DenyLag: 20},
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Enabled: true, Status: "ok", Lag: 3, Diverged: true}},
				Allowed:    true,
			},
			want: []string{"3 commit(s) behind", ", diverged"},
		},
		{
			desc: "a target whose last publication failed",
			status: mirrorStatus{
				Repository: "shop.git",
				Policy:     mirrorStatusPolicy{Required: true},
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Enabled: true, Status: "failed", Error: "the host refused"}},
				Allowed:    true,
			},
			want: []string{"the host refused"},
		},
		{
			desc: "a policy that is off",
			status: mirrorStatus{
				Repository: "shop.git",
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Enabled: true, Status: "ok"}},
				Allowed:    true,
			},
			want: []string{"the mirror policy is off"},
		},
		{
			desc: "a policy without a refusal",
			status: mirrorStatus{
				Repository: "shop.git",
				Policy:     mirrorStatusPolicy{Required: true, WarnLag: 5},
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Enabled: true, Status: "ok"}},
				Allowed:    true,
			},
			want: []string{", refusal off"},
		},
		{
			desc: "a refusal",
			status: mirrorStatus{
				Repository: "shop.git",
				Policy:     mirrorStatusPolicy{Required: true, WarnLag: 5, DenyLag: 20},
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Enabled: true, Status: "ok", Lag: 30}},
				Allowed:    false,
				Reason:     "the mirror github is 30 commits behind",
			},
			want: []string{"a push would be refused"},
		},
		{
			desc: "a warning",
			status: mirrorStatus{
				Repository: "shop.git",
				Policy:     mirrorStatusPolicy{Required: true, WarnLag: 5, DenyLag: 20},
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Enabled: true, Status: "ok", Lag: 7}},
				Allowed:    true,
				Warning:    "the mirror github is 7 commits behind",
			},
			want: []string{"a push would go through: the mirror github is 7 commits behind"},
		},
		{
			desc: "a mirror behind but under the warn threshold",
			status: mirrorStatus{
				Repository: "shop.git",
				Branch:     "main",
				Policy:     mirrorStatusPolicy{Required: true, WarnLag: 10},
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main"}, Enabled: true, Status: "ok", Lag: 2}},
				Allowed:    true,
			},
			want: []string{"the mirrors are 2 commit(s) behind, below the warn threshold of 10"},
		},
		{
			desc: "a mirror behind without a warn threshold",
			status: mirrorStatus{
				Repository: "shop.git",
				Branch:     "main",
				Policy:     mirrorStatusPolicy{Required: true},
				Targets:    []mirrorStatusTarget{{Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main"}, Enabled: true, Status: "ok", Lag: 4}},
				Allowed:    true,
			},
			want: []string{"the mirrors are 4 commit(s) behind, and the policy sets no warn threshold"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var out bytes.Buffer
			mirrorStatusPrint(&out, tc.status)
			for _, want := range tc.want {
				require.Contains(t, out.String(), want)
			}
		})
	}
}

// TestMirrorStatusLag covers the lag the policy compares its thresholds with:
// a disabled target does not count, a target of another branch does not count
// and the furthest behind target wins.
func TestMirrorStatusLag(t *testing.T) {
	targets := []mirrorStatusTarget{
		{Name: "github", Branches: []string{"main"}, Enabled: true, Lag: 3},
		{Name: "gitee", Branches: []string{"main"}, Enabled: true, Lag: 7},
		{Name: "off", Branches: []string{"main"}, Enabled: false, Lag: 9},
		{Name: "release", Branches: []string{"release/*"}, Enabled: true, Lag: 5},
	}
	require.Equal(t, 7, mirrorStatusLag("main", targets))
	require.Equal(t, 7, mirrorStatusLag("", targets))
	require.Equal(t, 0, mirrorStatusLag("other", targets))
}

// TestDescribeGateRule covers every clause of the description of a rule, so
// that an operator reads the same rule the server enforces.
func TestDescribeGateRule(t *testing.T) {
	require.Equal(t, "only a land may update it", describeGateRule(&gateRule{}))
	require.Contains(t, describeGateRule(&gateRule{AllowDirectUpdate: true}), "a push may update it")
	require.Contains(t, describeGateRule(&gateRule{RequireReview: true}), "a review is required")
	require.Contains(t, describeGateRule(&gateRule{RequireApprovals: 2}), "2 approval(s) are required")
	require.Contains(t, describeGateRule(&gateRule{AllowStaleBase: true}), "a review based on an older tip is accepted")
	require.Contains(t, describeGateRule(&gateRule{RequireVerified: true}), "must cover the tip")
	require.Contains(t, describeGateRule(&gateRule{RequireVerifiedResult: true}), "must cover the commit the land produces")
	require.Contains(t, describeGateRule(&gateRule{RequireFullTree: true}), "must certify the whole tree of the commit it names")
	require.Contains(t, describeGateRule(&gateRule{RequireSuites: []string{"unit", "lint"}}), "must certify the suites unit, lint")
}

// TestPrintGateCheck covers the answers of the gate for a reader: the subject
// of the check, the verdict and the checkpoint the branch holds.
func TestPrintGateCheck(t *testing.T) {
	for _, tc := range []struct {
		desc string
		res  gateResult
		want []string
	}{
		{
			desc: "a branch that no rule governs",
			res:  gateResult{Branch: "main"},
			want: []string{"the branch main is not managed"},
		},
		{
			desc: "a land that may go through",
			res:  gateResult{Branch: "main", Managed: true, Allowed: true, Commit: "abc"},
			want: []string{"the change abc may land on main"},
		},
		{
			desc: "a land that may not go through",
			res:  gateResult{Branch: "main", Managed: true, Commit: "abc", Reason: "a review is required"},
			want: []string{"the change abc may not land on main: a review is required"},
		},
		{
			desc: "a direct update with a commit",
			res:  gateResult{Branch: "main", Managed: true, Allowed: true, Direct: true, Commit: "abc"},
			want: []string{"the direct update of main to abc may update main"},
		},
		{
			desc: "a direct update without a commit",
			res:  gateResult{Branch: "main", Managed: true, Allowed: true, Direct: true},
			want: []string{"a direct update of main may update main"},
		},
		{
			desc: "a checkpoint that certifies the whole tree",
			res:  gateResult{Branch: "main", Managed: true, Allowed: true, Checkpoint: "c1", FullTree: true},
			want: []string{"certifies the whole tree of the commit c1"},
		},
		{
			desc: "a checkpoint that certifies one tree",
			res:  gateResult{Branch: "main", Managed: true, Allowed: true, Checkpoint: "c1", CertifiedTree: "t1"},
			want: []string{"certifies only the tree t1 of the commit c1"},
		},
		{
			desc: "a checkpoint that certifies no tree",
			res:  gateResult{Branch: "main", Managed: true, Allowed: true, Checkpoint: "c1"},
			want: []string{"certifies no tree of the commit c1"},
		},
		{
			desc: "a checkpoint with suites",
			res:  gateResult{Branch: "main", Managed: true, Allowed: true, Checkpoint: "c1", CheckpointSuites: []string{"unit"}},
			want: []string{"certifies the suite unit"},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			var out bytes.Buffer
			printGateCheck(&out, tc.res)
			for _, want := range tc.want {
				require.Contains(t, out.String(), want)
			}
		})
	}
}

// TestReviewNames covers the names of the states, the verdicts and the classes
// of a review, and the note of the approval.
func TestReviewNames(t *testing.T) {
	require.Equal(t, "landed", reviewStateName(gitalypb.Review_STATE_LANDED))
	require.Equal(t, "closed", reviewStateName(gitalypb.Review_STATE_CLOSED))
	require.Equal(t, "open", reviewStateName(gitalypb.Review_STATE_OPEN_UNSPECIFIED))
	require.Equal(t, "changes requested", reviewVerdictName(gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES))
	require.Equal(t, "approved", reviewVerdictName(gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED))
	require.Equal(t, "comment_only", reviewClassName(gitalypb.ReviewFile_CHANGE_CLASS_COMMENT_ONLY))
	require.Equal(t, "docs", reviewClassName(gitalypb.ReviewFile_CHANGE_CLASS_DOCS))
	require.Equal(t, "tests", reviewClassName(gitalypb.ReviewFile_CHANGE_CLASS_TESTS))
	require.Equal(t, "generated", reviewClassName(gitalypb.ReviewFile_CHANGE_CLASS_GENERATED))
	require.Equal(t, "config", reviewClassName(gitalypb.ReviewFile_CHANGE_CLASS_CONFIG))
	require.Equal(t, "code", reviewClassName(gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED))
	require.Equal(t, "yes", approvedNote(true))
	require.Equal(t, "no", approvedNote(false))
}

// TestFormatReview covers the report of a review: its description, its files,
// the state of its requirements, its verdicts and its comments.
func TestFormatReview(t *testing.T) {
	review := &gitalypb.Review{
		Id:           "r1",
		State:        gitalypb.Review_STATE_OPEN_UNSPECIFIED,
		Title:        []byte("the change"),
		TargetBranch: []byte("main"),
		BaseCommitId: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		HeadCommitId: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Author:       "alice",
		Description:  []byte("first line\nsecond line"),
		Approved:     true,
		Files: []*gitalypb.ReviewFile{
			{Path: []byte("a/x.go"), Class: gitalypb.ReviewFile_CHANGE_CLASS_CODE_UNSPECIFIED},
		},
		Requirements: []*gitalypb.ReviewRequirement{
			{Rule: "review", Approvals: 2, RequiredApprovals: 2, Satisfied: true},
			{Rule: "verify", Approvals: 0, RequiredApprovals: 1},
		},
		Verdicts: []*gitalypb.ReviewVerdict{
			{Kind: gitalypb.ReviewVerdict_KIND_APPROVE_UNSPECIFIED, Actor: "bob", CommitId: "cccccccccccccccccccccccccccccccccccccccc", Current: true},
			{Kind: gitalypb.ReviewVerdict_KIND_REQUEST_CHANGES, Actor: "carol", CommitId: "dddddddddddddddddddddddddddddddddddddddd", Body: []byte("please fix")},
		},
		Comments: []*gitalypb.ReviewComment{
			{Actor: "dan", Path: []byte("a/x.go"), Line: 3, CommitId: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Body: []byte("here")},
			{Actor: "erin", CommitId: "ffffffffffffffffffffffffffffffffffffffff", Body: []byte("overall")},
		},
	}

	var out bytes.Buffer
	formatReview(&out, review)
	text := out.String()
	for _, want := range []string{
		"review r1 (open): the change",
		"target main",
		"approved: yes",
		"files:",
		"a/x.go",
		"requirements:",
		"review: 2 of 2 approvals (satisfied)",
		"verify: 0 of 1 approvals (unsatisfied)",
		"verdicts:",
		"approved by bob",
		"changes requested by carol",
		"(superseded)",
		"comments:",
		"dan a/x.go:3 on ",
		"erin on ",
	} {
		require.Contains(t, text, want)
	}
}

// TestPathListString covers the printable form of a repeatable flag, including
// the flag that was never set.
func TestPathListString(t *testing.T) {
	var p pathList
	require.Equal(t, "", p.String())
	p.Set("a")
	p.Set("b")
	require.Equal(t, "a,b", p.String())
}

// TestDisplayLines covers the text of another client: every line is displayed,
// and the lines after the first are indented.
func TestDisplayLines(t *testing.T) {
	require.Equal(t, "a\n  b", displayLines("a\nb", "  "))
	require.Equal(t, "", displayLines("", "  "))
}

// TestExactBytesAndLines covers the fields that keep the exact bytes of a text
// that is not valid UTF-8.
func TestExactBytesAndLines(t *testing.T) {
	require.Nil(t, exactBytes("plain"))
	require.Equal(t, []byte{0xff}, exactBytes(string([]byte{0xff})))
	require.Nil(t, exactLines([]string{"a", "b"}))
	require.Equal(t, [][]byte{[]byte("a"), []byte{0xff}}, exactLines([]string{"a", string([]byte{0xff})}))
	require.Equal(t, []string{}, nonNilStrings(nil))
	require.Equal(t, []string{"a"}, nonNilStrings([]string{"a"}))
}

// TestWriteSearchJSONReportsTheFailureOfTheOutput covers the JSON report of
// the search that cannot be written.
func TestWriteSearchJSONReportsTheFailureOfTheOutput(t *testing.T) {
	matches := searchMatchesForTest()
	require.Error(t, writeSearchJSON(errWriter{}, matches))
	require.NoError(t, writeSearchJSON(&bytes.Buffer{}, matches))
}

// searchMatchesForTest returns the matches of a search over one file: a match
// with a line of context on each side.
func searchMatchesForTest() []sdk.SearchMatch {
	return []sdk.SearchMatch{
		{Path: "a/x.txt", Line: 2, Text: "match", Before: []string{"before"}, After: []string{"after"}},
		{Path: "a/x.txt", Line: 9, Text: "far"},
	}
}

// TestWriteSearchTextReportsTheFailureOfTheOutputToo covers the text report
// of the search that cannot be written.
func TestWriteSearchTextReportsTheFailureOfTheOutputToo(t *testing.T) {
	matches := searchMatchesForTest()
	require.Error(t, writeSearchText(errWriter{}, matches, false))
	require.NoError(t, writeSearchText(&bytes.Buffer{}, matches, false))
}

// TestSearchTextPrintsTheContext covers the report of the search with the
// context lines around a match.
func TestSearchTextPrintsTheContext(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeSearchText(&out, searchMatchesForTest(), true))
	text := out.String()
	require.Contains(t, text, "a/x.txt-1-before")
	require.Contains(t, text, "a/x.txt:2:match")
	require.Contains(t, text, "a/x.txt-3-after")
	require.True(t, strings.Contains(text, "--\n"), "two distant matches are separated")
}
