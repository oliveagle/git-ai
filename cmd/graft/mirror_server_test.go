package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
)

// TestMirrorPushPublishesSelectedBranches drives the mirror against a real Git
// remote. The remote is another Git repository reached over the file protocol:
// the command knows nothing about any hosting provider's API.
func TestMirrorPushPublishesSelectedBranches(t *testing.T) {
	env := startServer(t)
	tip := env.seedMain(t, map[string]string{"top.txt": "top\n"})
	gittest.Exec(t, env.cfg, "-C", env.repoPath, "tag", "backup-main", tip.String())

	// The mirror has unrelated history on main. A backup must not overwrite it
	// just because a scheduled retry ran.
	remote := filepath.Join(t.TempDir(), "mirror.git")
	seed := filepath.Join(t.TempDir(), "seed")
	gittest.Exec(t, env.cfg, "init", "--bare", "--initial-branch=main", remote)
	gittest.Exec(t, env.cfg, "init", "--initial-branch=main", seed)
	gittest.Exec(t, env.cfg, "-C", seed, "config", "user.name", "Remote")
	gittest.Exec(t, env.cfg, "-C", seed, "config", "user.email", "remote@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "remote.txt"), []byte("remote\n"), 0o644))
	gittest.Exec(t, env.cfg, "-C", seed, "add", "remote.txt")
	gittest.Exec(t, env.cfg, "-C", seed, "commit", "-m", "remote history")
	gittest.Exec(t, env.cfg, "-C", seed, "push", remote, "main")
	divergent := strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", remote, "rev-parse", "refs/heads/main")))
	require.NotEqual(t, tip.String(), divergent)

	target := []string{"-address", env.addr, "-storage", env.storage, "--repo", env.relPath}
	code, stdout, stderr := runCLI(t, append([]string{
		"mirror", "push", "--url", remote, "--branch", "main",
	}, target...)...)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stdout, "kept the divergent ref refs/heads/main")
	require.Contains(t, stdout, "the mirror is incomplete")
	require.Equal(t, divergent, strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", remote, "rev-parse", "refs/heads/main"))))

	// Force is explicit, after a person has decided that the Graft branch is the
	// history to keep. The selected branch and the tag move; the traditional CI
	// of the mirror sees them without any provider API call.
	code, stdout, stderr = runCLI(t, append([]string{
		"mirror", "push", "--url", remote, "--branch", "main", "--force",
	}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "mirrored "+env.relPath)
	require.Equal(t, tip.String(), strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", remote, "rev-parse", "refs/heads/main"))))
	require.Equal(t, tip.String(), strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", remote, "rev-parse", "refs/tags/backup-main"))))

	// The next scheduled run is a no-op: the reconciliation is idempotent.
	code, stdout, stderr = runCLI(t, append([]string{
		"mirror", "push", "--url", remote, "--branch", "main", "--json",
	}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	var res mirrorResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &res))
	require.Equal(t, env.relPath, res.Repository)
	require.Equal(t, []string{"main"}, res.Branches)
	require.Empty(t, res.DivergentRefs)
}

// TestMirrorPushUsage refuses an unspecified or unsafe backup before the
// server is contacted.
func TestMirrorPushUsage(t *testing.T) {
	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{
			desc: "without a branch allowlist",
			args: []string{"mirror", "push", "--repo", "shop.git", "--url", "https://gitee.com/group/shop.git"},
			want: "requires --branch PATTERN",
		},
		{
			desc: "with both credentials",
			args: []string{
				"mirror", "push", "--repo", "shop.git", "--url", "https://gitee.com/group/shop.git",
				"--branch", "main", "--token-env", "GITEE_TOKEN", "--ssh-key", "/tmp/key",
			},
			want: "either --token-env or --ssh-key",
		},
		{
			desc: "with a token on plain HTTP",
			args: []string{
				"mirror", "push", "--repo", "shop.git", "--url", "http://gitee.com/group/shop.git",
				"--branch", "main",
			},
			want: "refuses to send GITEE_TOKEN over http://",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv("GITEE_TOKEN", "secret")
			code, _, stderr := runCLI(t, tc.args...)
			require.Equal(t, exitUsage, code)
			require.Contains(t, stderr, tc.want)
		})
	}
}

// TestMirrorCredentialsByHost checks that one command adapts to the host of the
// URL instead of having one command per hosting provider, and that a host
// without a convention still works.
func TestMirrorCredentialsByHost(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		url      string
		env      string
		username string
	}{
		{desc: "gitee", url: "https://gitee.com/group/shop.git", env: "GITEE_TOKEN", username: "oauth2"},
		{desc: "github", url: "https://github.com/group/shop.git", env: "GITHUB_TOKEN", username: "x-access-token"},
		{desc: "gitlab", url: "https://gitlab.com/group/shop", env: "GITLAB_TOKEN", username: "oauth2"},
		{desc: "self-hosted gitlab", url: "https://git.example.com/group/shop.git", env: "", username: "oauth2"},
		{desc: "ssh with a user", url: "ssh://git@gitee.com/group/shop.git", env: "GITEE_TOKEN", username: "oauth2"},
		{desc: "scp-like", url: "git@github.com:group/shop.git", env: "GITHUB_TOKEN", username: "x-access-token"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			got := mirrorDefaults(tc.url)
			require.Equal(t, tc.env, got.tokenEnv)
			require.Equal(t, tc.username, got.username)
		})
	}
}

// TestMirrorAuthorizationPrefersTheFlag checks that an explicit --username and
// --token-env win over the host convention, and that no header is built for a
// mirror that has no token in the environment.
func TestMirrorAuthorizationPrefersTheFlag(t *testing.T) {
	t.Setenv("COMPANY_GIT_TOKEN", "s3cret")

	f := &mirrorFlags{url: "https://github.com/group/shop.git", tokenEnv: "COMPANY_GIT_TOKEN", username: "bot"}
	header, err := mirrorAuthorization(f)
	require.NoError(t, err)
	require.Equal(t, "Basic Ym90OnMzY3JldA==", header, "the token is sent as Basic auth for the named user")

	// An SSH mirror uses the key, not a header.
	f = &mirrorFlags{url: "git@github.com:group/shop.git", sshKey: "/tmp/key"}
	header, err = mirrorAuthorization(f)
	require.NoError(t, err)
	require.Empty(t, header)

	// A public mirror has no token in the environment and needs no header.
	f = &mirrorFlags{url: "https://git.example.com/group/shop.git"}
	header, err = mirrorAuthorization(f)
	require.NoError(t, err)
	require.Empty(t, header)
}
