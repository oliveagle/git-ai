package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadStateRejectsWhatItCannotRead covers the state file that cannot be
// read or does not describe a workspace this version can use.
func TestLoadStateRejectsWhatItCannotRead(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.MkdirAll(statePath(root), 0o755), "a directory where the state file should be")
	_, err := loadState(root)
	require.Error(t, err)
	_, err = ReadMeta(root)
	require.Error(t, err)

	// A state without the files map is a workspace whose files are all unchanged.
	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath(root)), 0o755))
	require.NoError(t, os.WriteFile(statePath(root), []byte(`{"version":1}`), 0o600))
	st, err := loadState(root)
	require.NoError(t, err)
	require.NotNil(t, st.Files)

	// A sparse prefix that is not a repository path is refused.
	require.NoError(t, os.WriteFile(statePath(root), []byte(`{"version":1,"sparse":["../x"]}`), 0o600))
	_, err = loadState(root)
	require.ErrorContains(t, err, "sparse prefix")
}

// TestObjectCacheRefusesWhatItCannotAddress covers the cache of blobs: only a
// valid object ID names a location, and the failures of the file system are
// reported.
func TestObjectCacheRefusesWhatItCannotAddress(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, err := objectPath(root, "not-an-object")
	require.ErrorContains(t, err, "invalid object ID")
	_, ok, err := readCachedObject(root, "not-an-object")
	require.Error(t, err)
	require.False(t, ok)
	require.Error(t, writeCachedObject(root, "not-an-object", []byte("x")))

	// A directory where the cached blob should be is not a cached blob.
	oid := strings.Repeat("a", 40)
	path, err := objectPath(root, oid)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(path, 0o755))
	_, ok, err = readCachedObject(root, oid)
	require.Error(t, err)
	require.False(t, ok)

	// A root that is a file has no metadata directory to write into.
	file := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	require.Error(t, writeCachedObject(file, oid, []byte("x")))
	require.Error(t, writeFileAtomic(filepath.Join(file, "sub"), filepath.Join(file, "sub", "f"), []byte("x"), 0o600))
}

// TestWriteLineDiffFallsBackToAWholeReplacement covers the bound of the diff
// table: a pair of files that would need too many cells is shown as a whole
// replacement instead of a minimal diff.
func TestWriteLineDiffFallsBackToAWholeReplacement(t *testing.T) {
	t.Parallel()

	a := make([]string, 2001)
	n := make([]string, 2001)
	var b strings.Builder
	writeLineDiff(&b, a, n)

	require.Equal(t, 4002, strings.Count(b.String(), "\n"))
	require.True(t, strings.HasPrefix(b.String(), "-\n"), "every line of the old file is removed")
	require.Contains(t, b.String(), "+\n")
}

// TestCloneRefusesADirectoryThatIsNotEmpty covers the first check of Clone:
// the workspace root must be empty or absent, so that nothing is overwritten.
func TestCloneRefusesADirectoryThatIsNotEmpty(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "keep"), []byte("x"), 0o600))

	_, err := Clone(t.Context(), root, NewMemoryBackend(), CloneOptions{Branch: "main"})
	require.ErrorContains(t, err, "is not empty")
}
