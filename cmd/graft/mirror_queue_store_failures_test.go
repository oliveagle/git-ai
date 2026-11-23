package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMirrorQueueReportsAQueueItCannotRead covers every subcommand that reads
// the queue file before it does anything else: a file that is not a queue must
// be reported by each of them, never taken for an empty queue that the command
// would then rewrite.
func TestMirrorQueueReportsAQueueItCannotRead(t *testing.T) {
	dir := t.TempDir()
	queue := filepath.Join(dir, "mirror-queue.json")
	require.NoError(t, os.WriteFile(queue, []byte("{ not a queue"), 0o600))

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
	} {
		t.Run(args[0], func(t *testing.T) {
			code, _, stderr := runCLI(t, append(append([]string{"mirror", "queue"}, args...), "--queue-file", queue)...)
			require.Equal(t, exitError, code, "%s", stderr)
			require.Contains(t, stderr, "is not a queue")
		})
	}
}

// TestMirrorTaskReportsAQueueItCannotRead covers the task subcommands: the
// history of the queue is a part of the same file, so a file that is not a
// queue is reported there too.
func TestMirrorTaskReportsAQueueItCannotRead(t *testing.T) {
	dir := t.TempDir()
	queue := filepath.Join(dir, "mirror-queue.json")
	require.NoError(t, os.WriteFile(queue, []byte("{ not a queue"), 0o600))

	for _, args := range [][]string{
		{"list"},
		{"show", "task-1"},
	} {
		t.Run(args[0], func(t *testing.T) {
			code, _, stderr := runCLI(t, append(append([]string{"mirror", "task"}, args...), "--queue-file", queue)...)
			require.Equal(t, exitError, code, "%s", stderr)
			require.Contains(t, stderr, "is not a queue")
		})
	}
}
