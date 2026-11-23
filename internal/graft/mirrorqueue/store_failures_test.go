package mirrorqueue

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// requireWritableModes skips a test that makes a directory read-only to make a
// write fail: a platform that does not enforce the mode bits of a directory
// would let the write through.
func requireWritableModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the platform does not enforce the permission bits of a directory")
	}
}

// TestFileStoreReportsTheQueueItCannotReadOrWrite covers the file system
// failures of the queue: a file that cannot be read, a directory that cannot be
// created or used, and a target that cannot be replaced.
func TestFileStoreReportsTheQueueItCannotReadOrWrite(t *testing.T) {
	t.Parallel()

	// A directory where the queue file should be cannot be read as one.
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "queue.json"), 0o700))
	store := NewFileStore(filepath.Join(dir, "queue.json"))
	_, err := store.Load()
	require.ErrorContains(t, err, "cannot be read")
	require.ErrorContains(t, store.Update(func(*Queue) error { return nil }), "cannot be read")

	// A queue without a version is written with the version of this binary.
	q := New()
	q.Version = 0
	path := filepath.Join(t.TempDir(), "queue.json")
	require.NoError(t, NewFileStore(path).Save(q))
	require.EqualValues(t, queueVersion, q.Version)
	loaded, err := NewFileStore(path).Load()
	require.NoError(t, err)
	require.EqualValues(t, queueVersion, loaded.Version)

	// A parent that is a file has no directory to create.
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	err = NewFileStore(filepath.Join(file, "queue.json")).Save(New())
	require.ErrorContains(t, err, "cannot be created")

	// A target that is a non-empty directory cannot be replaced by the file.
	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, "keep"), nil, 0o600))
	err = NewFileStore(target).Save(New())
	require.ErrorContains(t, err, "cannot be written")

	// An update whose lock cannot be created reports it.
	err = NewFileStore(filepath.Join(t.TempDir(), "missing", "queue.json")).Update(func(*Queue) error { return nil })
	require.ErrorContains(t, err, "cannot be locked")
}

// TestFileStoreTakesOverAStaleLock covers the lock a process left behind: it is
// removed and the queue is updated.
func TestFileStoreTakesOverAStaleLock(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "queue.json")
	lock := path + ".lock"
	require.NoError(t, os.WriteFile(lock, []byte("1 dead\n"), 0o600))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(lock, old, old))

	require.NoError(t, NewFileStore(path).Update(func(q *Queue) error { return nil }))
	_, err := os.Stat(lock)
	require.ErrorIs(t, err, os.ErrNotExist, "the lock of the update is released")
}

// TestReleaseLockKeepsALockOfAnotherProcess covers the release of a lock: a
// lock that is gone is nothing to do, and a lock another process took over is
// left alone.
func TestReleaseLockKeepsALockOfAnotherProcess(t *testing.T) {
	t.Parallel()

	releaseLock(filepath.Join(t.TempDir(), "gone.lock"), "token")

	path := filepath.Join(t.TempDir(), "queue.json.lock")
	require.NoError(t, os.WriteFile(path, []byte("42 other\n"), 0o600))
	releaseLock(path, "mine")
	_, err := os.Stat(path)
	require.NoError(t, err, "the lock of the other process stays")

	require.NoError(t, os.WriteFile(path, []byte("42 mine\n"), 0o600))
	releaseLock(path, "mine")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestDefaultPathReportsAMissingHome covers the client without a home
// directory, which has nowhere to keep its queue.
func TestDefaultPathReportsAMissingHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	_, err := DefaultPath()
	require.ErrorContains(t, err, "no home to live in")
}

// TestFileStoreReportsAReadOnlyDirectory covers the write of a queue whose
// directory cannot be written.
func TestFileStoreReportsAReadOnlyDirectory(t *testing.T) {
	requireWritableModes(t)

	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := NewFileStore(filepath.Join(dir, "queue.json")).Save(New())
	require.Error(t, err)
}
