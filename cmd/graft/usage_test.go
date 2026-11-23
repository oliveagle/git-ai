package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageErrors refuses every command line that cannot be run as written.
// The refusal is checked before the workspace is opened and before the server
// is contacted, so a script that gets the command line wrong learns why
// without touching a repository.
func TestUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{desc: "clone without arguments", args: []string{"clone"}, want: "clone requires ADDR REPO DIR"},
		{
			desc: "clone with a branch and a commit",
			args: []string{"clone", "addr", "repo.git", "dir", "--branch", "main", "--at", "abc"},
			want: "either --branch or --at",
		},
		{desc: "status with an argument", args: []string{"status", "extra"}, want: "status takes no arguments"},
		{desc: "diff with an argument", args: []string{"diff", "extra"}, want: "diff takes no arguments"},
		{desc: "commit with an argument", args: []string{"commit", "extra"}, want: "commit takes flags only"},
		{desc: "commit without a message", args: []string{"commit"}, want: "commit requires -m MESSAGE"},
		{desc: "commit that resolves without a landing", args: []string{"commit", "-m", "x", "--resolve"}, want: "commit --resolve requires --land BRANCH"},
		{desc: "land with two branches", args: []string{"land", "one", "two"}, want: "land takes at most one BRANCH"},
		{desc: "land of a commit without a branch", args: []string{"land", "--commit", "abc"}, want: "land --commit requires BRANCH"},
		{
			desc: "land of a commit that aborts",
			args: []string{"land", "--commit", "abc", "main", "--abort"},
			want: "land --commit takes neither",
		},
		{desc: "land that aborts with a branch", args: []string{"land", "--abort", "main"}, want: "land --abort takes no BRANCH"},
		{
			desc: "land that writes conflicts without resolving",
			args: []string{"land", "--conflicts-dir", "/tmp/conflicts"},
			want: "land --conflicts-dir requires --resolve",
		},
		{
			desc: "land of a resolution that also resolves",
			args: []string{"land", "--resolved", "/tmp/resolution", "--resolve"},
			want: "land --resolved takes neither --resolve nor --conflicts-dir",
		},
		{
			desc: "land that is a dry run and resolves",
			args: []string{"land", "--dry-run", "--resolve"},
			want: "land --dry-run takes neither",
		},
		{
			desc: "land that expects a commit and is a dry run",
			args: []string{"land", "--expect", "abc", "--dry-run"},
			want: "land --expect takes no --dry-run",
		},
		{
			desc: "land that expects a commit and resolves",
			args: []string{"land", "--expect", "abc", "--resolve"},
			want: "land --expect is for a plain land",
		},
		{desc: "land that expects a commit without a date", args: []string{"land", "--expect", "abc"}, want: "land --expect requires --timestamp"},
		{desc: "land that is a dry run without a date", args: []string{"land", "--dry-run"}, want: "land --dry-run requires --timestamp"},
		{
			desc: "land that pins a date and resolves",
			args: []string{"land", "--resolve", "--timestamp", "2026-01-01T00:00:00Z"},
			want: "land --timestamp is for a plain land",
		},
		{
			desc: "land with a date that is not a date",
			args: []string{"land", "--timestamp", "yesterday"},
			want: "want an RFC 3339 date",
		},
		{desc: "update with an argument", args: []string{"update", "extra"}, want: "update takes no arguments"},
		{desc: "log with an argument", args: []string{"log", "extra"}, want: "log takes no arguments"},
		{desc: "watch with an argument", args: []string{"watch", "extra"}, want: "watch takes no arguments"},
		{desc: "blame without a path", args: []string{"blame"}, want: "blame requires PATH"},
		{desc: "history without a path", args: []string{"history"}, want: "history requires PATH"},
		{desc: "cat without a revision", args: []string{"cat"}, want: "cat requires REV:PATH"},
		{desc: "show without a revision", args: []string{"show"}, want: "show requires REV"},
		{desc: "search without a query", args: []string{"search"}, want: "search requires QUERY"},
		{desc: "search with a negative limit", args: []string{"search", "x", "-n", "-1"}, want: "-n must not be negative"},
		{desc: "search with a negative context", args: []string{"search", "x", "--context", "-1"}, want: "--context must not be negative"},
		{desc: "policy without a subcommand", args: []string{"policy"}, want: "policy"},
		{desc: "policy with an unknown subcommand", args: []string{"policy", "bogus"}, want: "unknown policy subcommand"},
		{desc: "policy check without a path", args: []string{"policy", "check"}, want: "policy"},
		{desc: "gate without a subcommand", args: []string{"gate"}, want: "gate"},
		{desc: "gate with an unknown subcommand", args: []string{"gate", "bogus"}, want: "unknown gate subcommand"},
		{desc: "gate check without a branch", args: []string{"gate", "check"}, want: "gate check requires --branch B"},
		{desc: "green without a subcommand", args: []string{"green"}, want: "green"},
		{desc: "green with an unknown subcommand", args: []string{"green", "bogus"}, want: "unknown green subcommand"},
		{desc: "green set without a branch", args: []string{"green", "set"}, want: "green set requires --branch B"},
		{
			desc: "green set without a commit",
			args: []string{"green", "set", "--branch", "main"},
			want: "green set requires --commit OID",
		},
		{
			desc: "green set with a suite that names a path",
			args: []string{"green", "set", "--branch", "main", "--commit", "abc", "--suite", "a/b"},
			want: "is not a valid suite name",
		},
		{desc: "green show without a branch", args: []string{"green", "show"}, want: "green show requires --branch B"},
		{desc: "review without a subcommand", args: []string{"review"}, want: "review"},
		{desc: "review with an unknown subcommand", args: []string{"review", "bogus"}, want: "unknown review subcommand"},
		{desc: "review create with an argument", args: []string{"review", "create", "extra"}, want: "review create takes flags only"},
		{desc: "review create without a branch", args: []string{"review", "create"}, want: "review create requires --branch B"},
		{
			desc: "review create without a title",
			args: []string{"review", "create", "--branch", "main"},
			want: "review create requires --title T",
		},
		{desc: "review comment without a body", args: []string{"review", "comment", "42"}, want: "review comment requires --body B"},
		{
			desc: "review comment with a line but no path",
			args: []string{"review", "comment", "42", "--body", "x", "--line", "5"},
			want: "review comment --line requires --path P",
		},
		{desc: "storage without a subcommand", args: []string{"storage"}, want: "storage"},
		{desc: "storage with an unknown subcommand", args: []string{"storage", "bogus"}, want: "unknown storage subcommand"},
		{desc: "upgrade with an argument", args: []string{"upgrade", "extra"}, want: "upgrade"},
		{desc: "version with an argument", args: []string{"version", "extra"}, want: "usage: graft version"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			require.Equal(t, exitUsage, code, stderr)
			require.Contains(t, stderr, tc.want)
		})
	}
}
