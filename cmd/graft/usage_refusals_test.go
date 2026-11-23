package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
)

// TestUsageErrorsOfEveryCommandLine refuses the command lines of every
// command that cannot be run as written. The arguments are checked before the
// server is contacted, so the refusal needs no server.
func TestUsageErrorsOfEveryCommandLine(t *testing.T) {
	useMemoryBackend(t, workspace.NewMemoryBackend())
	t.Setenv("GRAFT_ADDRESS", "")

	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{desc: "storage without a subcommand", args: []string{"storage"}, want: "storage requires a subcommand"},
		{desc: "storage with an unknown subcommand", args: []string{"storage", "bogus"}, want: "unknown storage subcommand"},
		{desc: "a storage import with an argument", args: []string{"storage", "import", "extra"}, want: "storage import takes flags only"},
		{desc: "a storage state with an argument", args: []string{"storage", "state", "extra"}, want: "storage state takes flags only"},
		{desc: "a storage verify with an argument", args: []string{"storage", "verify", "extra"}, want: "storage verify takes flags only"},
		{desc: "a storage archive with an argument", args: []string{"storage", "archive", "extra"}, want: "storage archive takes flags only"},
		{desc: "a storage unarchive with an argument", args: []string{"storage", "unarchive", "extra"}, want: "storage unarchive takes flags only"},
		{desc: "a storage archived with an argument", args: []string{"storage", "archived", "extra"}, want: "storage archived takes flags only"},
		{desc: "a storage archived with a repository", args: []string{"storage", "archived", "-repo", "shop.git"}, want: "takes no -repo"},
		{desc: "a storage cleanup with an argument", args: []string{"storage", "cleanup-start", "extra"}, want: "storage cleanup-start takes flags only"},
		{desc: "a storage purge with an argument", args: []string{"storage", "purge", "extra"}, want: "storage purge takes flags only"},
		{desc: "a storage import that would be a dry run", args: []string{"storage", "import", "--dry-run"}, want: "does not take --dry-run"},
		{desc: "a storage state that checks connectivity", args: []string{"storage", "state", "--connectivity"}, want: "does not take --connectivity"},
		{desc: "a storage import that collects the removed repositories", args: []string{"storage", "import", "--removed"}, want: "does not take --removed"},
		{desc: "a storage import with the token of the super admin", args: []string{"storage", "import", "-admin-token", "secret"}, want: "does not take --admin-token"},
		{desc: "a conflicting reference with two targets", args: []string{"storage", "delete-conflicting-ref", "a", "b", "c"}, want: "takes a reference and its target"},

		{desc: "review without a subcommand", args: []string{"review"}, want: "review requires a subcommand"},
		{desc: "review with an unknown subcommand", args: []string{"review", "bogus"}, want: "unknown review subcommand"},
		{desc: "a review create with an argument", args: []string{"review", "create", "extra"}, want: "review create takes flags only"},
		{desc: "a review create without a title", args: []string{"review", "create", "--branch", "main"}, want: "review create requires --title"},
		{desc: "a review create without a branch", args: []string{"review", "create", "--title", "T"}, want: "review create requires --branch"},
		{desc: "a review show without an ID", args: []string{"review", "show"}, want: "review show requires ID"},
		{desc: "a review list with an argument", args: []string{"review", "list", "extra"}, want: "review list takes flags only"},
		{desc: "a review comment without an ID", args: []string{"review", "comment"}, want: "review comment requires ID"},
		{desc: "a review comment without a body", args: []string{"review", "comment", "7"}, want: "review comment requires --body"},
		{desc: "a review comment with a line and no path", args: []string{"review", "comment", "7", "--body", "B", "--line", "3"}, want: "--line requires --path"},
		{desc: "a review comment with a line out of range", args: []string{"review", "comment", "7", "--body", "B", "--line", "-1", "--path", "p"}, want: "must be between 0 and"},
		{desc: "a review approve without an ID", args: []string{"review", "approve"}, want: "review approve requires ID"},
		{desc: "a review request-changes without an ID", args: []string{"review", "request-changes"}, want: "review request-changes requires ID"},
		{desc: "a review update without an ID", args: []string{"review", "update"}, want: "review update requires ID"},
		{desc: "a review close without an ID", args: []string{"review", "close"}, want: "review close requires ID"},

		{desc: "gate without a subcommand", args: []string{"gate"}, want: "gate requires a subcommand"},
		{desc: "gate with two subcommands", args: []string{"gate", "check", "rules"}, want: "usage: graft gate"},
		{desc: "gate with an unknown subcommand", args: []string{"gate", "bogus"}, want: "unknown gate subcommand"},
		{desc: "a gate check without a branch", args: []string{"gate", "check"}, want: "gate check requires --branch"},

		{desc: "green without a subcommand", args: []string{"green"}, want: "green requires a subcommand"},
		{desc: "green with two subcommands", args: []string{"green", "set", "show"}, want: "usage: graft green"},
		{desc: "green with an unknown subcommand", args: []string{"green", "bogus"}, want: "unknown green subcommand"},
		{desc: "a green set without a branch", args: []string{"green", "set"}, want: "green set requires --branch"},
		{desc: "a green set without a commit", args: []string{"green", "set", "--branch", "main"}, want: "green set requires --commit"},
		{
			desc: "a green set with a suite that is no name",
			args: []string{"green", "set", "--branch", "main", "--commit", "abc", "--suite", "a/b"},
			want: "is not a valid suite name",
		},
		{desc: "a green show without a branch", args: []string{"green", "show"}, want: "green show requires --branch"},

		{desc: "policy without a subcommand", args: []string{"policy"}, want: "policy requires a subcommand"},
		{desc: "policy with an unknown subcommand", args: []string{"policy", "bogus"}, want: "unknown policy subcommand"},
		{desc: "a policy check without a file", args: []string{"policy", "check"}, want: "usage: graft policy check"},
		{desc: "a policy check with two files", args: []string{"policy", "check", "a.toml", "b.toml"}, want: "usage: graft policy check"},
		{
			desc: "a policy check of a file that is no policy",
			args: []string{"policy", "check", "no-such-file.toml"},
			want: "no-such-file.toml",
		},

		{desc: "a repo create without a path", args: []string{"repo", "create"}, want: "repo create takes one repository path"},
		{desc: "a repo create with two paths", args: []string{"repo", "create", "a.git", "b.git"}, want: "repo create takes one repository path"},
		{
			desc: "a repo create with an unknown object format",
			args: []string{"repo", "create", "shop.git", "--object-format", "md5"},
			want: "--object-format must be sha1 or sha256",
		},
		{
			desc: "a repo create with a branch and no initial commit",
			args: []string{"repo", "create", "shop.git", "--branch", "dev"},
			want: "requires --initial-commit",
		},
		{desc: "a repo create without a server", args: []string{"repo", "create", "shop.git"}, want: "needs the server address"},

		{desc: "a search without a query", args: []string{"search"}, want: "search requires QUERY"},
		{desc: "a search with two queries", args: []string{"search", "a", "b"}, want: "search requires QUERY"},
		{desc: "a search with an empty query", args: []string{"search", ""}, want: "search requires a non-empty QUERY"},
		{desc: "a search with a negative limit", args: []string{"search", "a", "-n", "-1"}, want: "-n must not be negative"},
		{desc: "a search with a negative context", args: []string{"search", "a", "--context", "-1"}, want: "--context must not be negative"},
		{
			desc: "a search with a path that is no path",
			args: []string{"search", "a", "--path", "../x"},
			want: "--path",
		},
		{desc: "a show without a revision", args: []string{"show"}, want: "show requires REV"},
		{desc: "a show with two revisions", args: []string{"show", "a", "b"}, want: "show requires REV"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			require.Equal(t, exitUsage, code, stderr)
			require.Contains(t, stderr, tc.want)
		})
	}
}

// TestTheStorageCommandsNeedTheStorageCore covers the storage commands that
// cannot reach the storage core, because the backend of the workspace is not a
// server.
func TestTheStorageCommandsNeedTheStorageCore(t *testing.T) {
	t.Setenv("GRAFT_ADDRESS", "")
	dir := filepath.Join(t.TempDir(), "ws")
	clonedWorkspace(t, dir)

	for _, args := range [][]string{
		{"storage", "import", "-C", dir},
		{"storage", "state", "-C", dir},
		{"storage", "verify", "-C", dir},
		{"storage", "checkpoint", "-C", dir},
		{"storage", "archive", "-C", dir},
		{"storage", "unarchive", "-C", dir},
		{"storage", "cleanup-start", "-C", dir, "-admin-token", "secret"},
		{"storage", "purge", "-C", dir, "-admin-token", "secret"},
	} {
		code, _, stderr := runCLI(t, args...)
		require.Equal(t, exitError, code, args)
		require.Contains(t, stderr, "the backend cannot reach the storage core", args)
	}
}

// TestTheCleanupCommandsNeedTheTokenOfTheSuperAdmin covers the cleanup and
// the purge without the token of the super admin.
func TestTheCleanupCommandsNeedTheTokenOfTheSuperAdmin(t *testing.T) {
	useMemoryBackend(t, workspace.NewMemoryBackend())
	t.Setenv("GRAFT_ADDRESS", "")

	for _, args := range [][]string{
		{"storage", "cleanup-start"},
		{"storage", "purge"},
	} {
		code, _, stderr := runCLI(t, args...)
		require.Equal(t, exitUsage, code, args)
		require.Contains(t, stderr, "need -admin-token", args)
	}
}

// TestTheStorageWideCommandsNeedAServer covers the storage commands that work
// on a whole storage, without a workspace to name the server.
func TestTheStorageWideCommandsNeedAServer(t *testing.T) {
	useMemoryBackend(t, workspace.NewMemoryBackend())
	t.Setenv("GRAFT_ADDRESS", "")

	code, _, stderr := runCLI(t, "storage", "gc", "--removed")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "needs a workspace or -address")

	code, _, stderr = runCLI(t, "storage", "archived")
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr, "needs a workspace or -address")
}
