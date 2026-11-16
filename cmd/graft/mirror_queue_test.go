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

// mirrorRemote returns an empty bare repository that stands for the mirror of
// a target. The remote is reached over the file protocol, so the test does
// not need a hosting provider.
func mirrorRemote(t *testing.T, env serverEnv) string {
	t.Helper()

	remote := filepath.Join(t.TempDir(), "mirror.git")
	gittest.Exec(t, env.cfg, "init", "--bare", "--initial-branch=main",
		"--object-format="+gittest.DefaultObjectHash.Format, remote)
	return remote
}

// mirrorQueueFile returns the file the queue of a test is kept in.
func mirrorQueueFile(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "mirror-queue.json")
}

// TestMirrorQueuePublishesToItsTarget drives the queue against a real Git
// remote: a target is added, the queue is run, and the remote carries the
// branch of the graft repository.
func TestMirrorQueuePublishesToItsTarget(t *testing.T) {
	env := startServer(t)
	tip := env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "added the mirror target gitee")

	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "published "+env.relPath+" to "+remote+" (gitee)")
	require.Equal(t, tip.String(), strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", remote, "rev-parse", "refs/heads/main"))))

	// The run is a task of the history, so an operator sees what the queue did.
	code, stdout, stderr = runCLI(t, "mirror", "task", "list", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "gitee")
	require.Contains(t, stdout, "ok")
}

// TestMirrorQueueKeepsADivergentBranch checks the safety net of the queue: a
// remote branch with unrelated history is kept, and the run says so.
func TestMirrorQueueKeepsADivergentBranch(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	// The mirror holds a history of its own, which a retry must not erase.
	seed := filepath.Join(t.TempDir(), "seed")
	gittest.Exec(t, env.cfg, "init", "--initial-branch=main",
		"--object-format="+gittest.DefaultObjectHash.Format, seed)
	gittest.Exec(t, env.cfg, "-C", seed, "config", "user.name", "Remote")
	gittest.Exec(t, env.cfg, "-C", seed, "config", "user.email", "remote@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "remote.txt"), []byte("remote\n"), 0o644))
	gittest.Exec(t, env.cfg, "-C", seed, "add", "remote.txt")
	gittest.Exec(t, env.cfg, "-C", seed, "commit", "-m", "remote history")
	gittest.Exec(t, env.cfg, "-C", seed, "push", remote, "main")
	divergent := strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", remote, "rev-parse", "refs/heads/main")))

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stdout, "kept the divergent ref refs/heads/main")
	require.Equal(t, divergent, strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", remote, "rev-parse", "refs/heads/main"))))

	// The task of the history says what happened, so the operator does not
	// have to read the output of the run to know that the mirror is behind.
	code, stdout, stderr = runCLI(t, "mirror", "task", "list", "--queue-file", queue, "--json")
	require.Equal(t, exitOK, code, stderr)
	var tasks []mirrorQueueTask
	require.NoError(t, json.Unmarshal([]byte(stdout), &tasks))
	require.Len(t, tasks, 1)
	require.Equal(t, "diverged", tasks[0].Status)
	require.Equal(t, []string{"refs/heads/main"}, tasks[0].DivergentRefs)
}

// TestMirrorQueueRunsOnlyTheTargetsThatAreDue checks the schedule of the
// queue: a target that was published waits for its interval, and a target
// that a person names is published on the spot.
func TestMirrorQueueRunsOnlyTheTargetsThatAreDue(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "1h", "--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "every 1h0m0s")

	// The target was published, so it waits for its interval.
	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "published "+env.relPath)

	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "no mirror target is due")

	// A person asks for the target by name, and the queue publishes it.
	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--target", "gitee", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "published "+env.relPath)
}

// TestMirrorQueueHoldsNoSecret checks that the file of the queue names the
// variable that carries the token, and never carries the token itself.
func TestMirrorQueueHoldsNoSecret(t *testing.T) {
	env := startServer(t)
	queue := mirrorQueueFile(t)
	t.Setenv("GITEE_TOKEN", "s3cret-token-value")

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", "https://gitee.com/group/shop.git", "--branch", "main",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	data, err := os.ReadFile(queue)
	require.NoError(t, err)
	require.NotContains(t, string(data), "s3cret-token-value")
	require.Contains(t, string(data), "GITEE_TOKEN")
}

// TestMirrorQueueManagesItsTargets checks the management of the queue: the
// targets are listed, disabled, enabled and removed.
func TestMirrorQueueManagesItsTargets(t *testing.T) {
	env := startServer(t)
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "github",
		"--repo", env.relPath, "--url", "https://github.com/group/shop.git", "--branch", "main",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "list", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "github")
	require.Contains(t, stdout, "https://github.com/group/shop.git")

	code, stdout, stderr = runCLI(t, "mirror", "queue", "disable", "github", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "disabled the mirror target github")

	code, stdout, stderr = runCLI(t, "mirror", "queue", "list", "--queue-file", queue, "--json")
	require.Equal(t, exitOK, code, stderr)
	var targets []mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &targets))
	require.Len(t, targets, 1)
	require.False(t, targets[0].Enabled)

	code, stdout, stderr = runCLI(t, "mirror", "queue", "enable", "github", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "enabled the mirror target github")

	code, _, stderr = runCLI(t, "mirror", "queue", "remove", "github", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, "mirror", "queue", "list", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "the mirror queue is empty")
}

// TestMirrorQueueUsage refuses a command line that cannot be run as written,
// before the queue or the server is touched.
func TestMirrorQueueUsage(t *testing.T) {
	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{
			desc: "without a subcommand",
			args: []string{"mirror", "queue"},
			want: "mirror queue requires a subcommand",
		},
		{
			desc: "with an unknown subcommand",
			args: []string{"mirror", "queue", "publish"},
			want: "unknown mirror queue subcommand",
		},
		{
			desc: "without a repository",
			args: []string{"mirror", "queue", "add", "gitee", "--url", "https://gitee.com/group/shop.git", "--branch", "main"},
			want: "requires --repo REPO",
		},
		{
			desc: "without a branch",
			args: []string{"mirror", "queue", "add", "gitee", "--repo", "shop.git", "--url", "https://gitee.com/group/shop.git"},
			want: "requires --branch PATTERN",
		},
		{
			desc: "with two credentials",
			args: []string{
				"mirror", "queue", "add", "gitee", "--repo", "shop.git", "--url", "https://gitee.com/group/shop.git",
				"--branch", "main", "--token-env", "GITEE_TOKEN", "--ssh-key", "/tmp/key",
			},
			want: "either --token-env or --ssh-key",
		},
		{
			desc: "with a token on plain HTTP",
			args: []string{
				"mirror", "queue", "add", "gitee", "--repo", "shop.git", "--url", "http://gitee.com/group/shop.git",
				"--branch", "main",
			},
			want: "refuses to send GITEE_TOKEN over http://",
		},
		{
			desc: "without the name of a target",
			args: []string{"mirror", "queue", "remove"},
			want: "takes the name of the target",
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

// TestMirrorTaskThatIsNotThere refuses a task of the history that the queue
// does not have.
func TestMirrorTaskThatIsNotThere(t *testing.T) {
	code, _, stderr := runCLI(t, "mirror", "task", "show", "task-1", "--queue-file", mirrorQueueFile(t))
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "has no task")
}
