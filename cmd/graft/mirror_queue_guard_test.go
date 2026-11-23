package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
)

// writeMirrorQueue writes a queue file the tests can drive a command with.
func writeMirrorQueue(t *testing.T, path string, queue *mirrorqueue.Queue) {
	t.Helper()

	data, err := json.Marshal(queue)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

// queueWithTarget returns a queue that holds one target.
func queueWithTarget(target *mirrorqueue.Target) *mirrorqueue.Queue {
	return &mirrorqueue.Queue{Version: 1, Targets: []*mirrorqueue.Target{target}}
}

// TestMirrorQueueRefusesAnIncompleteDeclaration covers the declarations the
// queue refuses before it touches the file or the server.
func TestMirrorQueueRefusesAnIncompleteDeclaration(t *testing.T) {
	t.Setenv("GITEE_TOKEN", "secret")

	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{
			desc: "no repository",
			args: []string{"mirror", "queue", "add", "gitee", "--url", "https://gitee.com/x/y.git", "--branch", "main"},
			want: "requires --repo REPO",
		},
		{
			desc: "no branch",
			args: []string{"mirror", "queue", "add", "gitee", "--repo", "shop.git", "--url", "https://gitee.com/x/y.git"},
			want: "requires --branch PATTERN",
		},
		{
			desc: "both credentials",
			args: []string{
				"mirror", "queue", "add", "gitee", "--repo", "shop.git",
				"--url", "https://gitee.com/x/y.git", "--branch", "main",
				"--token-env", "GITEE_TOKEN", "--ssh-key", "/tmp/key",
			},
			want: "either --token-env or --ssh-key",
		},
		{
			desc: "a token over http",
			args: []string{
				"mirror", "queue", "add", "gitee", "--repo", "shop.git",
				"--url", "http://gitee.example/x/y.git", "--branch", "main",
				"--token-env", "GITEE_TOKEN",
			},
			want: "refuses to send GITEE_TOKEN over http",
		},
		{
			desc: "update without a repository",
			args: []string{"mirror", "queue", "update", "gitee", "--url", "https://gitee.com/x/y.git", "--branch", "main"},
			want: "requires --repo REPO",
		},
		{
			desc: "update with both credentials",
			args: []string{
				"mirror", "queue", "update", "gitee", "--repo", "shop.git",
				"--url", "https://gitee.com/x/y.git", "--branch", "main",
				"--token-env", "GITEE_TOKEN", "--ssh-key", "/tmp/key",
			},
			want: "either --token-env or --ssh-key",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			code, _, stderr := runCLI(t, append(tc.args, "--queue-file", mirrorQueueFile(t))...)
			require.Equal(t, exitUsage, code, stderr)
			require.Contains(t, stderr, tc.want)
		})
	}
}

// TestMirrorQueueShowPrintsTheCredentialAndTheOutcome covers the fields of a
// target that the human-readable show prints.
func TestMirrorQueueShowPrintsTheCredentialAndTheOutcome(t *testing.T) {
	queue := mirrorQueueFile(t)
	next := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{Version: 1, Targets: []*mirrorqueue.Target{
		{
			Name:       "gitee",
			Repository: "shop.git",
			URL:        "https://gitee.com/x/y.git",
			Branches:   []string{"main"},
			TokenEnv:   "GITEE_TOKEN",
			Username:   "oauth2",
			Force:      true,
			Interval:   time.Hour,
			Enabled:    true,
			NextRun:    next,
			LastStatus: mirrorqueue.TaskFailed,
			LastError:  "the host refused the push",
		},
		{
			Name:       "gitee-ssh",
			Repository: "shop.git",
			URL:        "ssh://git@gitee.com/x/y.git",
			Branches:   []string{"main"},
			SSHKey:     "/home/user/.ssh/id_ed25519",
			Enabled:    true,
		},
	}})

	code, stdout, stderr := runCLI(t, "mirror", "queue", "show", "gitee", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "next run: "+next.Format(time.RFC3339))
	require.Contains(t, stdout, "token: GITEE_TOKEN")
	require.Contains(t, stdout, "username: oauth2")
	require.Contains(t, stdout, "force: yes")
	require.Contains(t, stdout, "last: failed (the host refused the push)")

	code, stdout, stderr = runCLI(t, "mirror", "queue", "show", "gitee-ssh", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "ssh key: /home/user/.ssh/id_ed25519")
}

// TestMirrorQueueReportsAnUnreadableFile covers the file failures every
// command of the queue reports instead of an answer.
func TestMirrorQueueReportsAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()

	notJSON := filepath.Join(dir, "not-json.json")
	require.NoError(t, os.WriteFile(notJSON, []byte("{not json"), 0o644))

	wrongVersion := filepath.Join(dir, "version.json")
	require.NoError(t, os.WriteFile(wrongVersion, []byte(`{"version":9,"targets":[],"tasks":[]}`), 0o644))

	for _, tc := range []struct {
		desc string
		file string
		want string
	}{
		{desc: "a directory", file: dir, want: "mirror-queue.json"},
		{desc: "a file that is not JSON", file: notJSON, want: "not-json.json"},
		{desc: "a file of another version", file: wrongVersion, want: "version 9"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			code, _, stderr := runCLI(t, "mirror", "queue", "list", "--queue-file", tc.file)
			require.Equal(t, exitError, code, stderr)
			require.NotEmpty(t, stderr)
		})
	}
}

// TestMirrorTaskCommandsRefuseBadInput covers the dispatcher and the usage
// refusals of graft mirror task.
func TestMirrorTaskCommandsRefuseBadInput(t *testing.T) {
	for _, tc := range []struct {
		desc string
		args []string
		want string
	}{
		{desc: "no subcommand", args: []string{"mirror", "task"}, want: "requires a subcommand"},
		{desc: "unknown subcommand", args: []string{"mirror", "task", "bogus"}, want: "unknown mirror task subcommand"},
		{desc: "list with an argument", args: []string{"mirror", "task", "list", "extra"}, want: "takes no arguments"},
		{desc: "show without an identifier", args: []string{"mirror", "task", "show"}, want: "takes the identifier of the task"},
		{desc: "show with two identifiers", args: []string{"mirror", "task", "show", "a", "b"}, want: "takes the identifier of the task"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			code, _, stderr := runCLI(t, tc.args...)
			require.Equal(t, exitUsage, code, stderr)
			require.Contains(t, stderr, tc.want)
		})
	}
}

// TestMirrorTaskHistory covers the empty history and the history that holds a
// failure, a divergence and a task that is still running.
func TestMirrorTaskHistory(t *testing.T) {
	queue := mirrorQueueFile(t)

	code, stdout, stderr := runCLI(t, "mirror", "task", "list", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "has no task yet")

	started := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	finished := started.Add(time.Second)
	writeMirrorQueue(t, queue, &mirrorqueue.Queue{
		Version: 1,
		Tasks: []*mirrorqueue.Task{
			{ID: "task-ok", Target: "gitee", Repository: "shop.git", URL: "https://gitee.com/x/y.git", Status: mirrorqueue.TaskOK, CreatedAt: started, StartedAt: started, FinishedAt: finished},
			{ID: "task-failed", Target: "github", Repository: "shop.git", URL: "https://github.com/x/y.git", Status: mirrorqueue.TaskFailed, CreatedAt: started, Error: "the host refused"},
			{ID: "task-diverged", Target: "gitee", Repository: "shop.git", URL: "https://gitee.com/x/y.git", Status: mirrorqueue.TaskDiverged, CreatedAt: started, DivergentRefs: []string{"refs/heads/main"}},
		},
	})

	code, stdout, stderr = runCLI(t, "mirror", "task", "list", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "task-ok")
	require.Contains(t, stdout, "the host refused")
	require.Contains(t, stdout, "refs/heads/main")

	code, stdout, stderr = runCLI(t, "mirror", "task", "list", "--json", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	var listed []mirrorQueueTask
	require.NoError(t, json.Unmarshal([]byte(stdout), &listed))
	require.Len(t, listed, 3)

	code, stdout, stderr = runCLI(t, "mirror", "task", "show", "task-ok", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "started: "+started.Format(time.RFC3339))
	require.Contains(t, stdout, "finished: "+finished.Format(time.RFC3339))

	code, stdout, stderr = runCLI(t, "mirror", "task", "show", "task-diverged", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "kept the divergent ref refs/heads/main")

	code, stdout, stderr = runCLI(t, "mirror", "task", "show", "task-failed", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "error: the host refused")

	code, stdout, stderr = runCLI(t, "mirror", "task", "show", "task-ok", "--json", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	var shown mirrorQueueTask
	require.NoError(t, json.Unmarshal([]byte(stdout), &shown))
	require.Equal(t, "task-ok", shown.ID)

	code, _, stderr = runCLI(t, "mirror", "task", "show", "ghost", "--queue-file", queue)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stderr, "has no task")
}

// TestMirrorQueueNamesTheServerItNeeds covers a target whose server is named
// neither by the target nor by the run.
func TestMirrorQueueNamesTheServerItNeeds(t *testing.T) {
	queue := mirrorQueueFile(t)
	writeMirrorQueue(t, queue, queueWithTarget(&mirrorqueue.Target{
		Name:       "gitee",
		Repository: "shop.git",
		URL:        "https://gitee.com/x/y.git",
		Branches:   []string{"main"},
		Enabled:    true,
	}))

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--target", "gitee", "--queue-file", queue, "--json")
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, failedTaskError(t, stdout), "names no server")

	code, _, stderr = runCLI(t, "mirror", "queue", "register", "gitee", "--queue-file", queue)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stderr, "names no server")

	code, _, stderr = runCLI(t, "mirror", "queue", "remove", "gitee", "--queue-file", queue)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stderr, "names no server")
}

// TestMirrorQueueRefusesACleartextTokenAtRunTime covers the second check of a
// token over http://, made when the task runs against the environment of the
// worker rather than the one of the operator who added the target.
func TestMirrorQueueRefusesACleartextTokenAtRunTime(t *testing.T) {
	queue := mirrorQueueFile(t)
	writeMirrorQueue(t, queue, queueWithTarget(&mirrorqueue.Target{
		Name:       "gitee",
		Repository: "shop.git",
		Address:    "127.0.0.1:1",
		URL:        "http://gitee.example/x/y.git",
		Branches:   []string{"main"},
		TokenEnv:   "GITEE_TOKEN",
		Enabled:    true,
	}))
	t.Setenv("GITEE_TOKEN", "secret")

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--target", "gitee", "--queue-file", queue, "--json")
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, failedTaskError(t, stdout), "refuses to send GITEE_TOKEN over http")
}

// TestMirrorQueueReportsAMissingCredentialFile covers a target whose SSH
// credential path does not hold a file.
func TestMirrorQueueReportsAMissingCredentialFile(t *testing.T) {
	env := startServer(t)
	queue := mirrorQueueFile(t)
	writeMirrorQueue(t, queue, queueWithTarget(&mirrorqueue.Target{
		Name:       "gitee",
		Repository: env.relPath,
		Address:    env.addr,
		Storage:    env.storage,
		URL:        "ssh://git@gitee.com/x/y.git",
		Branches:   []string{"main"},
		SSHKey:     filepath.Join(t.TempDir(), "missing-key"),
		Enabled:    true,
	}))

	code, stdout, stderr := runCLI(t, "mirror", "queue", "run", "--target", "gitee", "--queue-file", queue, "--json")
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, failedTaskError(t, stdout), "missing-key")
}

// failedTaskError returns the error of the one task a failed run reports.
func failedTaskError(t *testing.T, stdout string) string {
	t.Helper()

	var tasks []mirrorQueueTask
	require.NoError(t, json.Unmarshal([]byte(stdout), &tasks))
	require.Len(t, tasks, 1)
	return tasks[0].Error
}

// TestMirrorQueuePublishesWithADefaultConcurrency covers a run that leaves the
// concurrency out, or that asks for no concurrency at all.
func TestMirrorQueuePublishesWithADefaultConcurrency(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, "mirror", "queue", "run", "--concurrency", "0", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)

	code, _, stderr = runCLI(t, "mirror", "queue", "run", "--concurrency", "-1", "--queue-file", queue, "--all")
	require.Equal(t, exitOK, code, stderr)
}

// TestMirrorQueueWorkerKeepsRunningAfterAFailure covers the worker: a run that
// fails is reported on stderr and the worker waits for the next tick instead
// of stopping, and a tick that is not set takes the default.
func TestMirrorQueueWorkerKeepsRunningAfterAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stderr bytes.Buffer
	worker := &mirrorQueueFlags{queueFile: t.TempDir(), tick: -1}
	err := mirrorQueueWorker(ctx, io.Discard, &options{stderr: &stderr}, &mirrorFlags{}, worker, nil)
	require.NoError(t, err)
	require.Contains(t, stderr.String(), "graft mirror queue worker")
}
