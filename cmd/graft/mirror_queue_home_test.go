package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
)

// TestMirrorQueueNeedsAHomeForTheQueue covers every subcommand that resolves
// the queue file of the user: a process without a home and without
// --queue-file has no file to name, and every command says so instead of
// taking an empty path for a queue.
func TestMirrorQueueNeedsAHomeForTheQueue(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	for _, args := range [][]string{
		{"add", "gitee", "--repo", "shop.git", "--url", "https://gitee.com/x/y.git", "--branch", "main"},
		{"update", "gitee", "--repo", "shop.git", "--url", "https://gitee.com/x/y.git", "--branch", "main"},
		{"remove", "gitee"},
		{"enable", "gitee"},
		{"disable", "gitee"},
		{"list"},
		{"show", "gitee"},
		{"idle", "--idle-interval", "10m"},
		{"tier", "--active", "1h", "--recent", "3h", "--dormant", "12h"},
		{"run", "--all"},
		{"register"},
	} {
		t.Run(args[0], func(t *testing.T) {
			code, _, stderr := runCLI(t, append(append([]string{"mirror", "queue"}, args...), "-address", "tcp://127.0.0.1:1")...)
			require.Equal(t, exitError, code, "%s", stderr)
			require.Contains(t, stderr, "has no home to live in")
		})
	}

	for _, args := range [][]string{{"list"}, {"show", "task-1"}} {
		t.Run("task "+args[0], func(t *testing.T) {
			code, _, stderr := runCLI(t, append([]string{"mirror", "task"}, args...)...)
			require.Equal(t, exitError, code, "%s", stderr)
			require.Contains(t, stderr, "has no home to live in")
		})
	}
}

// queueOfTargets returns a queue the tests can drive the commands with.
func queueOfTargets(targets ...*mirrorqueue.Target) *mirrorqueue.Queue {
	return &mirrorqueue.Queue{Version: 1, Targets: targets}
}

// TestMirrorQueueTierKeepsWhatItCannotRead covers the targets whose repository
// nobody can read: a server that cannot be dialed, a target that names no
// server, a repository read twice by two targets, and a target the operator
// did not name. Each of them keeps the tier it has, and the command says how
// many it kept instead of failing.
func TestMirrorQueueTierKeepsWhatItCannotRead(t *testing.T) {
	queue := mirrorQueueFile(t)
	target := func(name, repo, address string) *mirrorqueue.Target {
		return &mirrorqueue.Target{
			Name:       name,
			Repository: repo,
			URL:        "https://gitee.com/x/y.git",
			Branches:   []string{"main"},
			Interval:   5 * time.Minute,
			Enabled:    true,
			Address:    address,
		}
	}
	writeMirrorQueue(t, queue, queueOfTargets(
		target("undialable", "shop.git", "not-an-address"),
		target("shares-the-repository", "shop.git", "not-an-address"),
		target("nameless-server", "other.git", ""),
		target("silent-server", "third.git", "tcp://127.0.0.1:1"),
	))

	code, stdout, stderr := runCLI(t, "mirror", "queue", "tier",
		"--active", "1h", "--recent", "3h", "--dormant", "12h", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "tiers 0 target(s): 4 kept")

	// Naming one target leaves the others alone.
	code, stdout, stderr = runCLI(t, "mirror", "queue", "tier", "undialable",
		"--active", "1h", "--recent", "3h", "--dormant", "12h", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "tiers 0 target(s): 1 kept")
}

// TestMirrorQueueUpdateAndRemoveReportAServerThatIsGone covers the two
// commands that write the queue first and then talk to the server: the local
// queue keeps the change, and the command reports the server it could not
// reach instead of pretending it succeeded.
func TestMirrorQueueUpdateAndRemoveReportAServerThatIsGone(t *testing.T) {
	queue := mirrorQueueFile(t)
	writeMirrorQueue(t, queue, queueOfTargets(&mirrorqueue.Target{
		Name:       "gitee",
		Repository: "shop.git",
		URL:        "https://gitee.com/x/y.git",
		Branches:   []string{"main"},
		Interval:   5 * time.Minute,
		Enabled:    true,
	}))

	code, _, stderr := runCLI(t, "mirror", "queue", "update", "gitee",
		"--repo", "shop.git", "--url", "https://gitee.com/x/z.git", "--branch", "main",
		"--queue-file", queue, "-address", "tcp://127.0.0.1:1")
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stderr, `registering the mirror target "gitee"`)

	code, _, stderr = runCLI(t, "mirror", "queue", "remove", "gitee", "--queue-file", queue, "-address", "tcp://127.0.0.1:1")
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stderr, `unregistering the mirror target "gitee"`)

	// The queue is the record of the client: update and remove happened even
	// though the server did not answer.
	code, stdout, stderr := runCLI(t, "mirror", "queue", "list", "--queue-file", queue)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "the mirror queue is empty")
}

// TestMirrorQueueTierReportsTheTargetsItMoved covers the JSON report of a run
// that moved a target: the operator sees what changed, not only how many.
func TestMirrorQueueTierReportsTheTargetsItMoved(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	landChange(t, env)
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, _, stderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "5m", "--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "tier", "--json", "--queue-file", queue,
		"-address", env.addr, "--active", "1h", "--recent", "3h", "--dormant", "12h",
		"--active-within", "24h", "--recent-within", "168h")
	require.Equal(t, exitOK, code, stderr)
	var moved []mirrorQueueTarget
	require.NoError(t, json.Unmarshal([]byte(stdout), &moved))
	require.Len(t, moved, 1)
	require.Equal(t, "gitee", moved[0].Name)
	require.Equal(t, "1h0m0s", moved[0].IdleInterval)
}

// TestMirrorQueueTierReportsAQueueItCannotWrite covers the tier run that read
// the ages of the repositories and then cannot store the tiers it derived: the
// failure is reported, not swallowed.
func TestMirrorQueueTierReportsAQueueItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	queue := filepath.Join(dir, "mirror-queue.json")
	writeMirrorQueue(t, queue, queueOfTargets(&mirrorqueue.Target{
		Name: "gitee", Repository: "shop.git", URL: "https://gitee.com/x/y.git",
		Branches: []string{"main"}, Interval: 5 * time.Minute, Enabled: true,
	}))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	code, _, stderr := runCLI(t, "mirror", "queue", "tier",
		"--active", "1h", "--recent", "3h", "--dormant", "12h", "--queue-file", queue)
	require.Equal(t, exitError, code, stderr)
	require.NotEmpty(t, stderr)
}

// TestMirrorQueueRegisterReportsWhatItRegistered covers the two answers of
// register: a queue it cannot read is reported, and a registration the server
// accepted is named for every target.
func TestMirrorQueueRegisterReportsWhatItRegistered(t *testing.T) {
	dir := t.TempDir()
	notAQueue := filepath.Join(dir, "not-a-queue.json")
	require.NoError(t, os.WriteFile(notAQueue, []byte("{ not a queue"), 0o600))

	code, _, stderr := runCLI(t, "mirror", "queue", "register", "--queue-file", notAQueue)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stderr, "is not a queue")

	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	queue := mirrorQueueFile(t)
	code, _, stderr = runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", mirrorRemote(t, env), "--branch", "main",
		"--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := runCLI(t, "mirror", "queue", "register", "--queue-file", queue,
		"-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "registered the mirror target gitee of "+env.relPath)
}
