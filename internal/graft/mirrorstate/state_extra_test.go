package mirrorstate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
)

// TestTargetValidate covers the targets a repository cannot publish: one
// without a URL, one without a branch pattern and one with an empty pattern.
func TestTargetValidate(t *testing.T) {
	require.NoError(t, mirrorstate.Target{Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main"}}.Validate())

	err := mirrorstate.Target{Name: "github", Branches: []string{"main"}}.Validate()
	require.ErrorContains(t, err, "needs a URL")

	err = mirrorstate.Target{Name: "github", URL: "https://github.com/g/s.git"}.Validate()
	require.ErrorContains(t, err, "at least one branch pattern")

	err = mirrorstate.Target{Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main", " "}}.Validate()
	require.ErrorContains(t, err, "is empty")

	err = mirrorstate.Target{Name: "a/b", URL: "https://github.com/g/s.git", Branches: []string{"main"}}.Validate()
	require.ErrorContains(t, err, "not a plain name")
}

// TestSyncValidate covers the states of a synchronisation the store cannot
// keep: one without a target, one whose target is a path and one with an
// unknown status.
func TestSyncValidate(t *testing.T) {
	require.NoError(t, mirrorstate.Sync{Target: "github", Status: mirrorstate.SyncOK}.Validate())

	err := mirrorstate.Sync{Status: mirrorstate.SyncOK}.Validate()
	require.ErrorContains(t, err, "needs the name of its target")

	err = mirrorstate.Sync{Target: "a/b", Status: mirrorstate.SyncOK}.Validate()
	require.ErrorContains(t, err, "not a plain name")

	err = mirrorstate.Sync{Target: "github"}.Validate()
	require.ErrorContains(t, err, "needs a status")

	err = mirrorstate.Sync{Target: "github", Status: "bogus"}.Validate()
	require.ErrorContains(t, err, "unknown status")
}

// TestTargetMatchesTheEmptyPattern covers the pattern that names no branch.
func TestTargetMatchesTheEmptyPattern(t *testing.T) {
	target := mirrorstate.Target{Branches: []string{"", "main"}}
	require.True(t, target.MatchesBranch("main"))
	require.False(t, target.MatchesBranch("other"))

	empty := mirrorstate.Target{Branches: []string{""}}
	require.False(t, empty.MatchesBranch("main"))
}

// TestFileStoreRefusesWhatItCannotDecode covers the files of the state that
// are not a target or a synchronisation, and the files the store does not
// read at all.
func TestFileStoreRefusesWhatItCannotDecode(t *testing.T) {
	repoPath := t.TempDir()
	store := mirrorstate.Open(repoPath)

	targets := filepath.Join(repoPath, "graft", "mirror", "targets")
	require.NoError(t, os.MkdirAll(targets, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targets, "broken.json"), []byte("{"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(targets, "notes.txt"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(targets, "a-directory.json"), 0o755))

	_, err := store.ListTargets(context.Background(), mirrorstate.Key{})
	require.ErrorContains(t, err, "decode the mirror target")

	syncs := filepath.Join(repoPath, "graft", "mirror", "syncs")
	require.NoError(t, os.MkdirAll(syncs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(syncs, "broken.json"), []byte("{"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(syncs, "notes.txt"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(syncs, "a-directory.json"), 0o755))

	// A target the store cannot decode stops the read of every target.
	_, err = store.ListTargets(context.Background(), mirrorstate.Key{})
	require.Error(t, err)
}

// TestFileStoreSkipsTheFilesOfOtherExtensions covers the directory that holds
// a file the store does not read: only the targets and the states count.
func TestFileStoreSkipsTheFilesOfOtherExtensions(t *testing.T) {
	repoPath := t.TempDir()
	store := mirrorstate.Open(repoPath)

	targets := filepath.Join(repoPath, "graft", "mirror", "targets")
	require.NoError(t, os.MkdirAll(targets, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targets, "notes.txt"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(targets, "a-directory.json"), 0o755))

	list, err := store.ListTargets(context.Background(), mirrorstate.Key{})
	require.NoError(t, err)
	require.Empty(t, list)

	syncs := filepath.Join(repoPath, "graft", "mirror", "syncs")
	require.NoError(t, os.MkdirAll(syncs, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(syncs, "notes.txt"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(syncs, "a-directory.json"), 0o755))

	states, err := store.ListSyncs(context.Background(), mirrorstate.Key{})
	require.NoError(t, err)
	require.Empty(t, states)
}

// TestFileStoreReportsAStateDirectoryItCannotCreate covers the repository
// whose state directory cannot be created because a file is in its place.
func TestFileStoreReportsAStateDirectoryItCannotCreate(t *testing.T) {
	repoPath := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repoPath, "graft"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "graft", "mirror"), []byte("x"), 0o644))

	store := mirrorstate.Open(repoPath)
	err := store.RegisterTarget(context.Background(), mirrorstate.Key{}, mirrorstate.Target{
		Name: "github", URL: "https://github.com/g/s.git", Branches: []string{"main"},
	})
	require.ErrorContains(t, err, "create the mirror state directory")
}

// TestFileStoreReportsATargetItCannotRemove covers the target whose file is
// not a file any more.
func TestFileStoreReportsATargetItCannotRemove(t *testing.T) {
	repoPath := t.TempDir()
	store := mirrorstate.Open(repoPath)

	targets := filepath.Join(repoPath, "graft", "mirror", "targets")
	require.NoError(t, os.MkdirAll(filepath.Join(targets, "github.json", "inner"), 0o755))

	err := store.UnregisterTarget(context.Background(), mirrorstate.Key{}, "github")
	require.ErrorContains(t, err, "remove the mirror target")
}
