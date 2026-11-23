package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	require.Contains(t, stdout, "published "+env.relPath+" to "+remote+" (gitee,")
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

// TestMirrorQueueIdleSetsTheFallbackOfTheTargets checks the command that
// moves a queue whose targets were written before the idle interval
// existed: it sets the fallback and leaves the rest of the target alone.
func TestMirrorQueueIdleSetsTheFallbackOfTheTargets(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "5m", "--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "idle", "--idle-interval", "1h",
		"--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "the mirror target gitee now waits 1h0m0s")

	targets := mirrorQueueTargetState(t, queue, env)
	require.Len(t, targets, 1)
	require.Equal(t, "1h0m0s", targets[0].IdleInterval)
	require.Equal(t, "5m0s", targets[0].Interval)

	// A fallback shorter than the schedule it backs up is refused.
	code, _, stderr = runCLI(t, "mirror", "queue", "idle", "--idle-interval", "1m",
		"--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "shorter than the interval")
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

// TestMirrorQueueFailsWithTheNameOfTheToken checks that a task whose token is
// not in the environment of the worker says which variable is missing, and
// not the error of the host.
func TestMirrorQueueFailsWithTheNameOfTheToken(t *testing.T) {
	env := startServer(t)
	queue := mirrorQueueFile(t)
	t.Setenv("GITEE_TOKEN", "s3cret-token-value")

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", "https://gitee.com/group/shop.git", "--branch", "main",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	// The worker runs on a machine that has no token for the mirror.
	require.NoError(t, os.Unsetenv("GITEE_TOKEN"))

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitError, code)
	require.Contains(t, stdout, "failed to publish")
	require.Contains(t, stdout, "GITEE_TOKEN is not set in the environment")
	require.Contains(t, stderr, "mirror tasks did not publish")
}

// TestMirrorQueueKeepsATargetWithoutAScheduleOutOfTheSchedule checks that a
// target with no interval is only published when a person names it.
func TestMirrorQueueKeepsATargetWithoutAScheduleOutOfTheSchedule(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "0", "--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "every manual")

	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "no mirror target is due")

	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--target", "gitee", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "published "+env.relPath)
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

// TestMirrorQueueNamesTheFileOfAnEmptyQueue checks that a run which published
// nothing says which file it read. The refusal a pusher reads sends them to
// this command, and the file of the queue is named by a flag, then by the
// environment, then by the default of the user: an empty queue of the wrong
// user must not read as a queue with nothing to do.
func TestMirrorQueueNamesTheFileOfAnEmptyQueue(t *testing.T) {
	env := startServer(t)
	queue := mirrorQueueFile(t)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "holds no target")
	require.Contains(t, stdout, queue)
	require.Contains(t, stdout, "GRAFT_MIRROR_QUEUE")
}

// TestMirrorQueueUpdatesATargetInPlace checks that a declaration which changed
// is restated where the target already is: the schedule and the branches the
// queue and the server hold change, and what the last publication did stays.
func TestMirrorQueueUpdatesATargetInPlace(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)
	common := []string{"--queue-file", queue, "-address", env.addr, "-storage", env.storage}

	code, _, stderr := runCLI(t, append([]string{
		"mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main", "--interval", "1h",
	}, common...)...)
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, append([]string{"mirror", "queue", "run", "--target", "gitee"}, common...)...)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, append([]string{
		"mirror", "queue", "update", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main", "--branch", "release/*",
		"--interval", "5m0s",
	}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "updated the mirror target gitee")
	require.Contains(t, stdout, "every 5m0s")
	require.Contains(t, stdout, "enabled")

	// The queue holds one target, with the new declaration and the outcome of
	// the publication it already had.
	code, stdout, stderr = runCLI(t, append([]string{"mirror", "queue", "show", "gitee", "--json"}, common...)...)
	require.Equal(t, exitOK, code, stderr)
	var target mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &target))
	require.Equal(t, "5m0s", target.Interval)
	require.Equal(t, []string{"main", "release/*"}, target.Branches)
	require.Equal(t, "ok", target.LastStatus)
	require.NotEmpty(t, target.LastTask)

	// The server decides the policy on the branches it holds for the target,
	// so the new pattern reaches it too.
	code, stdout, stderr = runCLI(t, append([]string{
		"mirror", "status", "--repo", env.relPath, "--branch", "release/1.0",
		"--json",
	}, "-address", env.addr, "-storage", env.storage)...)
	require.Equal(t, exitOK, code, stderr)
	require.NotContains(t, stderr, "error")
	require.NotContains(t, stdout, "not published to any mirror target")
}

// landChange lands a change on the main branch of the repository of the test,
// through the server, so that the changeset log of the repository records it.
func landChange(t *testing.T, env serverEnv) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "ws")
	code, _, stderr := runCLI(t, "clone", "-storage", env.storage, env.addr, env.relPath, dir)
	require.Equal(t, exitOK, code, stderr)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "top.txt"), []byte("changed\n"), 0o644))
	code, _, stderr = runCLI(t, "commit", "-C", dir, "-author", "Dev <dev@example.com>",
		"-m", "change top", "-land", "main")
	require.Equal(t, exitOK, code, stderr)
}

// mirrorQueueTargetState returns the targets of the queue as the command
// prints them.
func mirrorQueueTargetState(t *testing.T, queue string, env serverEnv) []mirrorQueueTarget {
	t.Helper()

	code, stdout, stderr := runCLI(t, "mirror", "queue", "list", "--queue-file", queue,
		"-address", env.addr, "--json")
	require.Equal(t, exitOK, code, stderr)
	var targets []mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &targets))
	return targets
}

// TestMirrorQueuePublishesOnAChange checks the event side of the queue: a
// repository that changed is published before its interval has passed,
// because the changeset log of the repository is the event the queue waits
// for, and the interval is only the fallback.
func TestMirrorQueuePublishesOnAChange(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "1h", "--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "published "+env.relPath)

	// The target waits an hour, so a run publishes nothing.
	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "no mirror target is due")

	// The repository changes: the log of the repository moves, and the queue
	// publishes the change without waiting for the interval.
	landChange(t, env)
	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "published "+env.relPath)
	require.Contains(t, stdout, "on a change")
}

// TestMirrorQueueStretchesTheIntervalOfAQuietTarget checks the fallback: a
// target whose repository did not change waits longer after every run, up to
// its idle interval, and a change puts it back on its interval.
func TestMirrorQueueStretchesTheIntervalOfAQuietTarget(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "5m", "--idle-interval", "1h",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "published "+env.relPath)

	targets := mirrorQueueTargetState(t, queue, env)
	require.Len(t, targets, 1)
	require.Equal(t, "1h0m0s", targets[0].IdleInterval)
	require.Equal(t, 1, targets[0].QuietRuns, "a mirror that found nothing new is a quiet mirror")
	next, err := time.Parse(time.RFC3339, targets[0].NextRun)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(10*time.Minute), next, time.Minute)

	// A run that finds the mirror up to date stretches the delay again: the
	// delay of ten minutes becomes twenty.
	code, _, stderr = runCLI(t, "mirror", "queue", "run", "--all", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)

	targets = mirrorQueueTargetState(t, queue, env)
	require.Len(t, targets, 1)
	require.Equal(t, 2, targets[0].QuietRuns)
	next, err = time.Parse(time.RFC3339, targets[0].NextRun)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(20*time.Minute), next, time.Minute)

	// A change puts the target back on its interval: it is active again.
	landChange(t, env)
	code, _, stderr = runCLI(t, "mirror", "queue", "run", "--all", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)

	targets = mirrorQueueTargetState(t, queue, env)
	require.Len(t, targets, 1)
	require.Equal(t, 0, targets[0].QuietRuns)
	next, err = time.Parse(time.RFC3339, targets[0].NextRun)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(5*time.Minute), next, time.Minute)
}

// TestMirrorQueueDoesNotReplayTheNewestChangeOfARepository checks the first
// observation: a repository whose log already moved — an import, a clone, a
// repository that changed while the queue was down — is published once on its
// schedule and not again as if its newest entry were a change. The cursor has
// to start at the tail of the log for that, not one entry short of it.
func TestMirrorQueueDoesNotReplayTheNewestChangeOfARepository(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	// The log moves before the queue ever reads it.
	landChange(t, env)
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "1h", "--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "published "+env.relPath)
	require.Contains(t, stdout, "on the schedule")

	// Nothing moved since that publication, and the newest entry of the log
	// belongs to the history the queue started from.
	code, stdout, stderr = runCLI(t, "mirror", "queue", "run", "--queue-file", queue, "-address", env.addr)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "no mirror target is due")
	require.NotContains(t, stdout, "on a change")
}
