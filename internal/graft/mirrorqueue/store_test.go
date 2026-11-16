package mirrorqueue

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFileStoreLoadOfAQueueThatWasNeverWritten(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "queue.json"))
	q, err := store.Load()
	require.NoError(t, err)
	require.Empty(t, q.Targets)
	require.Empty(t, q.Tasks)
}

func TestFileStoreSavesAndLoadsTheQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	store := NewFileStore(path)

	q := New()
	target := newTarget("gitee")
	target.Interval = 5 * time.Minute
	require.NoError(t, q.AddTarget(target))
	q.AddTask(&Task{ID: "task-1", Target: "gitee", Status: TaskOK})
	require.NoError(t, store.Save(q))

	loaded, err := store.Load()
	require.NoError(t, err)
	require.Len(t, loaded.Targets, 1)
	require.Equal(t, "gitee", loaded.Targets[0].Name)
	require.Equal(t, 5*time.Minute, loaded.Targets[0].Interval)
	require.Len(t, loaded.Tasks, 1)
	require.Equal(t, "task-1", loaded.Tasks[0].ID)

	// The queue is a private file of the operator.
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFileStoreWritesTheQueueNextToItself(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.json")
	store := NewFileStore(path)

	q := New()
	require.NoError(t, q.AddTarget(newTarget("gitee")))
	require.NoError(t, store.Save(q))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the queue is written in one piece, so no half-written file is left behind")
	require.Equal(t, "queue.json", entries[0].Name())
}

func TestFileStoreRefusesAQueueThatIsNotAQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	require.NoError(t, os.WriteFile(path, []byte("this is not a queue"), 0o600))

	store := NewFileStore(path)
	_, err := store.Load()
	require.ErrorContains(t, err, "is not a queue")
}

func TestFileStoreRefusesAQueueOfAnUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"version": 99, "targets": [], "tasks": []}`), 0o600))

	store := NewFileStore(path)
	_, err := store.Load()
	require.ErrorContains(t, err, "version 99")
}

func TestFileStoreUpdateKeepsBothChangesOfTwoProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	store := NewFileStore(path)

	var wg sync.WaitGroup
	for _, name := range []string{"gitee", "github"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_ = store.Update(func(q *Queue) error {
				return q.AddTarget(newTarget(name))
			})
		}(name)
	}
	wg.Wait()

	q, err := store.Load()
	require.NoError(t, err)
	require.Len(t, q.Targets, 2, "a change of one process is not lost by the change of another")
}

func TestFileStoreUpdateDoesNotWriteWhenTheChangeFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.json")
	store := NewFileStore(path)

	require.Error(t, store.Update(func(q *Queue) error {
		require.NoError(t, q.AddTarget(newTarget("gitee")))
		return errors.New("the change did not finish")
	}), "the change that failed is reported")

	q, err := store.Load()
	require.NoError(t, err)
	require.Empty(t, q.Targets, "the change that failed is not written")

	require.NoError(t, store.Update(func(q *Queue) error {
		return q.AddTarget(newTarget("gitee"))
	}), "the change that failed was not written, so the target is not there yet")

	q, err = store.Load()
	require.NoError(t, err)
	require.Len(t, q.Targets, 1)

	require.Error(t, store.Update(func(q *Queue) error {
		return q.AddTarget(newTarget("gitee"))
	}), "the target is there now, so the second one is refused")
	q, err = store.Load()
	require.NoError(t, err)
	require.Len(t, q.Targets, 1)
}

func TestDefaultPathIsInTheDirectoryOfTheUser(t *testing.T) {
	path, err := DefaultPath()
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(path, filepath.Join("graft", "mirror-queue.json")), path)
}
