package verify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadStateReportsAFileItCannotRead covers a state path that exists and
// is not a file the process can read: the failure names the file, instead of
// reading it as the initial state.
func TestLoadStateReportsAFileItCannotRead(t *testing.T) {
	_, err := loadState(t.TempDir())
	require.ErrorContains(t, err, "read state file")
}

// TestSaveStateReportsWhereItCannotWrite covers the two steps of the atomic
// save that the file system can refuse: the temporary file, and the
// replacement of the state.
func TestSaveStateReportsWhereItCannotWrite(t *testing.T) {
	t.Run("the temporary file", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o500))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

		err := saveState(filepath.Join(dir, "state.json"), State{})
		require.ErrorContains(t, err, "create temporary state file")
	})

	t.Run("the replacement", func(t *testing.T) {
		// The state path is a directory: the temporary file is written, and
		// replacing the directory with it is refused.
		path := filepath.Join(t.TempDir(), "state.json")
		require.NoError(t, os.Mkdir(path, 0o755))

		err := saveState(path, State{})
		require.ErrorContains(t, err, "replace state file")
	})
}

// TestPrepareWorkDirReportsWhatItCannotPrepare covers the work directory the
// verifier cannot prepare: one whose parent is a file, and one the process may
// not look into.
func TestPrepareWorkDirReportsWhatItCannotPrepare(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("not a directory"), 0o644))
	require.ErrorContains(t, prepareWorkDir(filepath.Join(file, "work")), "create work_dir")

	closed := filepath.Join(t.TempDir(), "closed")
	require.NoError(t, os.Mkdir(closed, 0o000))
	t.Cleanup(func() { _ = os.Chmod(closed, 0o700) })
	require.Error(t, prepareWorkDir(closed))
}
