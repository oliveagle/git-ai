package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// TestMirrorQueueWorkerPublishesAndStops runs the worker of the queue: it
// publishes the targets that are due, over and over, and it stops when the
// command is interrupted.
func TestMirrorQueueWorkerPublishesAndStops(t *testing.T) {
	env := startServer(t)
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	remote := mirrorRemote(t, env)
	queue := mirrorQueueFile(t)

	code, _, addStderr := runCLI(t, "mirror", "queue", "add", "gitee",
		"--repo", env.relPath, "--url", remote, "--branch", "main",
		"--interval", "50ms", "--queue-file", queue, "-address", env.addr, "-storage", env.storage)
	require.Equal(t, exitOK, code, addStderr)

	ctx, cancel := context.WithCancel(testhelper.Context(t))
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runContext(ctx, []string{
			"mirror", "queue", "worker", "--tick", "100ms", "--queue-file", queue, "-address", env.addr,
		}, &stdout, &stderr)
	}()

	// The worker publishes the target that is due, without waiting for a
	// person to run the queue.
	require.Eventually(t, func() bool {
		return mirrorQueueHasTask(t, queue, mirrorqueue.TaskOK)
	}, 30*time.Second, 100*time.Millisecond, "the worker published the target")

	cancel()
	select {
	case code := <-done:
		require.Equal(t, exitOK, code, "an interrupted worker stops without a failure")
	case <-time.After(30 * time.Second):
		t.Fatal("the worker did not stop")
	}
}

// mirrorQueueHasTask tells whether the queue of a test holds a task with that
// status.
func mirrorQueueHasTask(t *testing.T, path, status string) bool {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	queue := &mirrorqueue.Queue{}
	if err := json.Unmarshal(data, queue); err != nil {
		return false
	}
	for _, task := range queue.Tasks {
		if task.Status == status {
			return true
		}
	}
	return false
}
