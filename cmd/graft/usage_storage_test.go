package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUsageErrorsOfTheStorageAndMirrorCommands refuses the command lines of
// the storage, mirror and repository commands that cannot be run as written.
func TestUsageErrorsOfTheStorageAndMirrorCommands(t *testing.T) {
	t.Setenv("GRAFT_ADDRESS", "")
	t.Setenv("GITEE_TOKEN", "secret")

	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{
			desc: "a checkpoint with an argument",
			args: []string{"storage", "checkpoint", "extra"},
			want: "storage checkpoint takes flags only",
		},
		{
			desc: "a checkpoint that would be a dry run",
			args: []string{"storage", "checkpoint", "--dry-run"},
			want: "does not take --dry-run",
		},
		{
			desc: "an archive that checks connectivity",
			args: []string{"storage", "archive", "--connectivity"},
			want: "does not take --connectivity",
		},
		{
			desc: "a garbage collection with an argument",
			args: []string{"storage", "gc", "extra"},
			want: "storage gc takes flags only",
		},
		{
			desc: "a garbage collection of removed repositories with a repository",
			args: []string{"storage", "gc", "--removed", "-repo", "shop.git"},
			want: "takes no -repo",
		},
		{
			desc: "a conflicting reference without its target",
			args: []string{"storage", "delete-conflicting-ref"},
			want: "takes a reference and its target",
		},
		{
			desc: "an archive with the token of the super admin",
			args: []string{"storage", "archive", "-admin-token", "secret"},
			want: "does not take --admin-token",
		},
		{desc: "mirror without a subcommand", args: []string{"mirror"}, want: "mirror"},
		{desc: "mirror with an unknown subcommand", args: []string{"mirror", "bogus"}, want: "unknown mirror subcommand"},
		{desc: "mirror push with an argument", args: []string{"mirror", "push", "extra"}, want: "mirror push"},
		{desc: "mirror push without a repository", args: []string{"mirror", "push"}, want: "mirror push requires --repo REPO"},
		{
			desc: "mirror push without a url",
			args: []string{"mirror", "push", "--repo", "shop.git"},
			want: "mirror push requires --url URL",
		},
		{
			desc: "mirror push without a branch",
			args: []string{"mirror", "push", "--repo", "shop.git", "--url", "https://gitee.com/group/shop.git"},
			want: "mirror push requires --branch PATTERN",
		},
		{
			desc: "mirror push with two credentials",
			args: []string{
				"mirror", "push", "--repo", "shop.git", "--url", "https://gitee.com/group/shop.git", "--branch", "main",
				"--token-env", "GITEE_TOKEN", "--ssh-key", "/tmp/key",
			},
			want: "either --token-env or --ssh-key",
		},
		{
			desc: "mirror push with a token over http",
			args: []string{
				"mirror", "push", "--repo", "shop.git", "--url", "http://gitee.com/group/shop.git", "--branch", "main",
				"--token-env", "GITEE_TOKEN",
			},
			want: "refuses to send GITEE_TOKEN over http://",
		},
		{desc: "mirror status with an argument", args: []string{"mirror", "status", "extra"}, want: "mirror status"},
		{desc: "mirror status without a repository", args: []string{"mirror", "status"}, want: "mirror status requires --repo REPO"},
		{
			desc: "mirror status with two branches",
			args: []string{"mirror", "status", "--repo", "shop.git", "--branch", "main", "--branch", "release"},
			want: "mirror status takes at most one --branch",
		},
		{desc: "repo without a subcommand", args: []string{"repo"}, want: "repo"},
		{desc: "repo with an unknown subcommand", args: []string{"repo", "bogus"}, want: "unknown repo subcommand"},
		{
			desc: "a policy check with no limit",
			args: []string{"policy", "check", "--limit", "0", "policy.toml"},
			want: "--limit must be positive",
		},
		{
			desc: "a policy check without a server",
			args: []string{"policy", "check", "-repo", "shop.git", "policy.toml"},
			want: "needs the server address",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			require.Equal(t, exitUsage, code, stderr)
			require.Contains(t, stderr, tc.want)
		})
	}
}
