package mirrorsweep

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
)

// The outcome of a run has to reach the queue.
//
// A target whose last run is not recorded stays "running" for ever, and the
// mirror of it is never published again. A queue that cannot be written is
// therefore an error of the run, and not something to swallow.
func TestTheOutcomeOfARunHasToReachTheQueue(t *testing.T) {
	dir := t.TempDir()
	store := mirrorqueue.NewFileStore(filepath.Join(dir, "queue.json"))
	queue := mirrorqueue.New()
	require.NoError(t, queue.AddTarget(&mirrorqueue.Target{
		Name:       "shop",
		Repository: "shop.git",
		URL:        "git@example.com:group/shop.git",
		Branches:   []string{"*"},
		Interval:   60_000_000_000,
	}))
	require.NoError(t, store.Save(queue))

	runner := &Runner{Store: store}
	target, ok := queue.Target("shop")
	require.True(t, ok)

	task := &mirrorqueue.Task{
		ID:      "task-1",
		Target:  "shop",
		Status:  mirrorqueue.TaskOK,
		Changed: true,
	}
	require.NoError(t, runner.record(target, task, true))

	stored, err := store.Load()
	require.NoError(t, err)
	current, ok := stored.Target("shop")
	require.True(t, ok)
	require.Equal(t, mirrorqueue.TaskOK, current.LastStatus)
	require.Equal(t, "task-1", current.LastTask)
}

// A queue that cannot be written says so, instead of leaving the target with
// the state it had.
func TestAQueueThatCannotBeWrittenIsAnError(t *testing.T) {
	store := mirrorqueue.NewFileStore(filepath.Join(t.TempDir(), "missing", "queue.json"))
	runner := &Runner{Store: store}

	target := &mirrorqueue.Target{
		Name:       "shop",
		Repository: "shop.git",
		URL:        "git@example.com:group/shop.git",
	}
	task := &mirrorqueue.Task{
		ID:     "task-1",
		Target: "shop",
		Status: mirrorqueue.TaskOK,
	}
	err := runner.record(target, task, true)
	require.Error(t, err)
}
