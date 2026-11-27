package mirrorsweep

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorqueue"
)

// A publication that a worker that is gone left running must be closed.
//
// The Temporal server retries the activity of such a run, so the work itself
// is not lost. What is lost is the truth of the queue: a task that never
// ended reads as a publication for ever, and the target of it reads as
// publishing for ever too.
func TestTheTaskAWorkerThatIsGoneLeftRunningIsClosed(t *testing.T) {
	dir := t.TempDir()
	store := mirrorqueue.NewFileStore(filepath.Join(dir, "queue.json"))
	queue := mirrorqueue.New()
	require.NoError(t, queue.AddTarget(&mirrorqueue.Target{
		Name:       "shop",
		Repository: "shop.git",
		URL:        "git@example.com:group/shop.git",
		Branches:   []string{"*"},
		Interval:   time.Minute,
	}))
	require.NoError(t, store.Save(queue))

	started := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	now := started.Add(10 * time.Minute)
	runner := &Runner{Store: store, Now: func() time.Time { return now }}

	stored, err := store.Load()
	require.NoError(t, err)
	target, ok := stored.Target("shop")
	require.True(t, ok)

	// The worker wrote that the run started, and then it was gone.
	require.NoError(t, runner.record(target, &mirrorqueue.Task{
		ID:         "task-1",
		Target:     "shop",
		Repository: "shop.git",
		URL:        "git@example.com:group/shop.git",
		Status:     mirrorqueue.TaskRunning,
		CreatedAt:  started,
		StartedAt:  started,
	}, false))

	stored, err = store.Load()
	require.NoError(t, err)
	running, ok := stored.Task("task-1")
	require.True(t, ok)
	require.Equal(t, mirrorqueue.TaskRunning, running.Status)

	// The reaper closes it.
	reaped, err := runner.Reap(time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, reaped)

	stored, err = store.Load()
	require.NoError(t, err)
	closed, ok := stored.Task("task-1")
	require.True(t, ok)
	require.Equal(t, mirrorqueue.TaskFailed, closed.Status)
	require.NotEmpty(t, closed.Error)
	require.False(t, closed.FinishedAt.IsZero())

	// The target says what happened to it, and waits for the backoff of a
	// failure like any other failure.
	current, ok := stored.Target("shop")
	require.True(t, ok)
	require.Equal(t, mirrorqueue.TaskFailed, current.LastStatus)
	require.NotEmpty(t, current.LastError)
	require.Equal(t, "task-1", current.LastTask)
	require.True(t, current.NextRun.After(now))
	require.True(t, current.NextRun.Before(now.Add(2*time.Minute)))
}

// A run that is still within its deadline may well be publishing right now,
// and the reaper must leave it alone.
func TestTheReaperLeavesARunThatIsStillWithinItsDeadlineAlone(t *testing.T) {
	dir := t.TempDir()
	store := mirrorqueue.NewFileStore(filepath.Join(dir, "queue.json"))
	queue := mirrorqueue.New()
	require.NoError(t, queue.AddTarget(&mirrorqueue.Target{
		Name:       "shop",
		Repository: "shop.git",
		URL:        "git@example.com:group/shop.git",
		Branches:   []string{"*"},
		Interval:   time.Minute,
	}))
	require.NoError(t, store.Save(queue))

	started := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	now := started.Add(10 * time.Second)
	runner := &Runner{Store: store, Now: func() time.Time { return now }}

	stored, err := store.Load()
	require.NoError(t, err)
	target, ok := stored.Target("shop")
	require.True(t, ok)
	require.NoError(t, runner.record(target, &mirrorqueue.Task{
		ID:        "task-1",
		Target:    "shop",
		Status:    mirrorqueue.TaskRunning,
		CreatedAt: started,
		StartedAt: started,
	}, false))

	reaped, err := runner.Reap(time.Minute)
	require.NoError(t, err)
	require.Equal(t, 0, reaped)

	stored, err = store.Load()
	require.NoError(t, err)
	running, ok := stored.Task("task-1")
	require.True(t, ok)
	require.Equal(t, mirrorqueue.TaskRunning, running.Status)
}

// A queue that holds no run that was interrupted has nothing to close.
func TestTheReaperClosesNothingWhenNothingWasInterrupted(t *testing.T) {
	dir := t.TempDir()
	store := mirrorqueue.NewFileStore(filepath.Join(dir, "queue.json"))
	require.NoError(t, store.Save(mirrorqueue.New()))

	runner := &Runner{Store: store}
	reaped, err := runner.Reap(time.Minute)
	require.NoError(t, err)
	require.Equal(t, 0, reaped)
}
