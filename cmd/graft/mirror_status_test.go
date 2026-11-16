package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/mirrorgate"
)

// mirrorStatusOf runs graft mirror status --json and returns what it printed.
func mirrorStatusOf(t *testing.T, env serverEnv, branch string, extra ...string) (int, mirrorStatus, string) {
	t.Helper()

	args := append([]string{
		"mirror", "status", "--repo", env.relPath, "--branch", branch, "--json",
		"-address", env.addr, "-storage", env.storage,
	}, extra...)
	code, stdout, stderr := runCLI(t, args...)

	var status mirrorStatus
	if stdout != "" {
		require.NoError(t, json.Unmarshal([]byte(stdout), &status), "stdout: %s", stdout)
	}
	return code, status, stderr
}

// TestMirrorStatusGuidesTheRepair walks the policy from "no mirror at all" to
// "a mirror far enough behind to refuse the push", and checks that the
// command tells an operator what to repair at every step.
func TestMirrorStatusGuidesTheRepair(t *testing.T) {
	env := startServer(t, func(cfg *config.Cfg) {
		cfg.Graft.Mirror = config.GraftMirror{Required: true, WarnLag: 10, DenyLag: 20}
	})
	tip := env.seedMain(t, map[string]string{"a.txt": "a\n"})

	queueFile := filepath.Join(t.TempDir(), "queue.json")
	remote := filepath.Join(t.TempDir(), "mirror.git")
	gittest.Exec(t, env.cfg, "init", "--bare", "--initial-branch=main",
		"--object-format="+gittest.DefaultObjectHash.Format, remote)

	common := []string{"-address", env.addr, "-storage", env.storage, "--queue-file", queueFile}

	// A repository that no mirror publishes takes no push, and the status
	// says which command declares the target.
	code, status, stderr := mirrorStatusOf(t, env, "main")
	require.Equal(t, exitError, code, stderr)
	require.False(t, status.Allowed)
	require.Equal(t, mirrorgate.CodeNoTarget, status.Code)

	// The queue registers the target with the server as it adds it.
	code, stdout, stderr := runCLI(t, append([]string{
		"mirror", "queue", "add", "codeup",
		"--repo", env.relPath, "--url", remote, "--branch", "main", "--manual",
	}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "added the mirror target codeup")

	// A target that never published is not a mirror yet.
	code, status, stderr = mirrorStatusOf(t, env, "main")
	require.Equal(t, exitError, code, stderr)
	require.False(t, status.Allowed)
	require.Equal(t, mirrorgate.CodeNeverSynced, status.Code)
	require.Len(t, status.Targets, 1)
	require.Empty(t, status.Targets[0].Status)

	// Publishing the target records what the mirror holds, and the push goes
	// through again.
	code, _, stderr = runCLI(t, append([]string{"mirror", "queue", "run", "--target", "codeup"}, common...)...)
	require.Equal(t, exitOK, code, stderr)

	code, status, stderr = mirrorStatusOf(t, env, "main")
	require.Equal(t, exitOK, code, stderr)
	require.True(t, status.Allowed)
	require.Equal(t, mirrorgate.CodeAllowed, status.Code)
	require.Len(t, status.Targets, 1)
	require.Equal(t, "ok", status.Targets[0].Status)
	require.Equal(t, 0, status.Targets[0].Lag)

	// A mirror ten commits behind is warned about, not refused.
	parent := tip
	for range 12 {
		parent = gittest.WriteCommit(t, env.cfg, env.repoPath,
			gittest.WithTree(env.writeTree(t, map[string]string{"a.txt": parent.String()})),
			gittest.WithParents(parent),
			gittest.WithBranch("main"),
		)
	}
	code, status, stderr = mirrorStatusOf(t, env, "main")
	require.Equal(t, exitOK, code, stderr)
	require.True(t, status.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, status.Code)
	require.Equal(t, 12, status.Targets[0].Lag)
	require.Contains(t, status.Warning, "12 commits behind")
	require.Contains(t, status.Warning, "graft mirror queue run")

	// A mirror twenty commits behind is refused: the next step of the policy.
	for range 8 {
		parent = gittest.WriteCommit(t, env.cfg, env.repoPath,
			gittest.WithTree(env.writeTree(t, map[string]string{"a.txt": parent.String()})),
			gittest.WithParents(parent),
			gittest.WithBranch("main"),
		)
	}
	code, status, stderr = mirrorStatusOf(t, env, "main")
	require.Equal(t, exitError, code, stderr)
	require.False(t, status.Allowed)
	require.Equal(t, mirrorgate.CodeLagging, status.Code)
	require.Contains(t, status.Reason, "20 commits hold a push back")

	// The branch the push writes is the branch the lag is measured for: the
	// mainline is behind, a topic branch no target publishes is not.
	code, status, stderr = mirrorStatusOf(t, env, "topic")
	require.Equal(t, exitOK, code, stderr)
	require.True(t, status.Allowed)
	require.Equal(t, mirrorgate.CodeBranchNotMirrored, status.Code)
	require.Contains(t, status.Warning, "not published to any mirror target")

	// Removing the target takes the repository back to "no mirror at all".
	code, _, stderr = runCLI(t, append([]string{"mirror", "queue", "remove", "codeup"}, common...)...)
	require.Equal(t, exitOK, code, stderr)

	code, status, stderr = mirrorStatusOf(t, env, "main")
	require.Equal(t, exitError, code, stderr)
	require.Equal(t, mirrorgate.CodeNoTarget, status.Code)
}

// TestMirrorStatusWithoutThePolicy checks that a server that does not
// configure the policy shows every mirror and refuses nothing.
func TestMirrorStatusWithoutThePolicy(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"a.txt": "a\n"})

	code, status, stderr := mirrorStatusOf(t, env, "main")
	require.Equal(t, exitOK, code, stderr)
	require.True(t, status.Allowed)
	require.Equal(t, mirrorgate.CodeAllowed, status.Code)
	require.False(t, status.Policy.Required)
	require.Empty(t, status.Targets)
}

// TestMirrorStatusNamesTheDistanceToTheMirror checks the sentence an operator
// reads while the mirrors fall behind: the number is shown before it reaches
// the threshold of the policy, so that the warning is never the first news of
// a mirror that is drifting.
func TestMirrorStatusNamesTheDistanceToTheMirror(t *testing.T) {
	env := startServer(t, func(cfg *config.Cfg) {
		cfg.Graft.Mirror = config.GraftMirror{Required: true, WarnLag: 10, DenyLag: 20}
	})
	tip := env.seedMain(t, map[string]string{"a.txt": "a\n"})

	queueFile := filepath.Join(t.TempDir(), "queue.json")
	remote := filepath.Join(t.TempDir(), "mirror.git")
	gittest.Exec(t, env.cfg, "init", "--bare", "--initial-branch=main",
		"--object-format="+gittest.DefaultObjectHash.Format, remote)

	common := []string{"-address", env.addr, "-storage", env.storage, "--queue-file", queueFile}
	code, _, stderr := runCLI(t, append([]string{
		"mirror", "queue", "add", "codeup",
		"--repo", env.relPath, "--url", remote, "--branch", "main", "--manual",
	}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	code, _, stderr = runCLI(t, append([]string{"mirror", "queue", "run", "--target", "codeup"}, common...)...)
	require.Equal(t, exitOK, code, stderr)

	gittest.WriteCommit(t, env.cfg, env.repoPath,
		gittest.WithTree(env.writeTree(t, map[string]string{"a.txt": tip.String()})),
		gittest.WithParents(tip),
		gittest.WithBranch("main"),
	)

	code, stdout, stderr := runCLI(t, append([]string{
		"mirror", "status", "--repo", env.relPath, "--branch", "main",
	}, "-address", env.addr, "-storage", env.storage)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "1 commit(s) behind")
	require.Contains(t, stdout, "below the warn threshold of 10")
}
