package mirrorstate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
)

// TestFileStoreWritesWithTheUsualModes pins the permissions of what a write
// creates: a state directory anyone in the group can read, and a lock file the
// owner writes and everyone reads.
func TestFileStoreWritesWithTheUsualModes(t *testing.T) {
	repoPath := t.TempDir()
	store := mirrorstate.Open(repoPath)
	require.NoError(t, store.RegisterTarget(context.Background(), mirrorstate.Key{}, mirrorstate.Target{
		Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main"},
	}))

	mirror := filepath.Join(repoPath, "graft", "mirror")
	for _, dir := range []string{mirror, filepath.Join(mirror, "targets")} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), dir)
	}

	lock, err := os.Stat(filepath.Join(mirror, ".lock"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), lock.Mode().Perm())
}

// TestFileStoreReportsALockItCannotTake covers the state of a repository whose
// lock is not a file any more: nothing is written.
func TestFileStoreReportsALockItCannotTake(t *testing.T) {
	repoPath := t.TempDir()
	mirror := filepath.Join(repoPath, "graft", "mirror")
	require.NoError(t, os.MkdirAll(filepath.Join(mirror, ".lock", "inner"), 0o755))

	store := mirrorstate.Open(repoPath)
	err := store.RegisterTarget(context.Background(), mirrorstate.Key{}, mirrorstate.Target{
		Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main"},
	})
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(mirror, "targets", "github.json"))
}

// TestFileStoreReportsAStateItCannotWrite covers the state directory that does
// not let the store create its temporary file.
func TestFileStoreReportsAStateItCannotWrite(t *testing.T) {
	repoPath := t.TempDir()
	targets := filepath.Join(repoPath, "graft", "mirror", "targets")
	require.NoError(t, os.MkdirAll(targets, 0o755))
	require.NoError(t, os.Chmod(targets, 0o555))
	t.Cleanup(func() { _ = os.Chmod(targets, 0o755) })

	store := mirrorstate.Open(repoPath)
	err := store.RegisterTarget(context.Background(), mirrorstate.Key{}, mirrorstate.Target{
		Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main"},
	})
	require.Error(t, err)
	require.NoFileExists(t, filepath.Join(targets, "github.json"))
}

// TestFileStoreReportsAStateItCannotPublish covers a target whose file is a
// directory: the rename fails and the temporary file is removed, so a later
// write starts clean.
func TestFileStoreReportsAStateItCannotPublish(t *testing.T) {
	repoPath := t.TempDir()
	targets := filepath.Join(repoPath, "graft", "mirror", "targets")
	require.NoError(t, os.MkdirAll(filepath.Join(targets, "github.json", "inner"), 0o755))

	store := mirrorstate.Open(repoPath)
	err := store.RegisterTarget(context.Background(), mirrorstate.Key{}, mirrorstate.Target{
		Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main"},
	})
	require.Error(t, err)

	entries, readErr := os.ReadDir(targets)
	require.NoError(t, readErr)
	for _, entry := range entries {
		require.NotContains(t, entry.Name(), ".tmp-", "a failed write leaves no temporary file")
	}
}

// TestTargetValidateNamesThePositionOfAnEmptyPattern checks the message of an
// empty branch pattern: a person fixes it by counting the patterns.
func TestTargetValidateNamesThePositionOfAnEmptyPattern(t *testing.T) {
	err := mirrorstate.Target{
		Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"", "main"},
	}.Validate()
	require.ErrorContains(t, err, "branch pattern 1")
}

// TestTargetValidateRejectsTheNamesThatCannotBeAFile checks the target names
// the state refuses because they are written in a path or a line of text.
func TestTargetValidateRejectsTheNamesThatCannotBeAFile(t *testing.T) {
	for _, name := range []string{".", "..", "a b", "a\tb"} {
		err := mirrorstate.Target{
			Name: name, URL: "https://github.com/g/s.git", Branches: []string{"main"},
		}.Validate()
		require.Error(t, err, "target name %q", name)
	}
}
