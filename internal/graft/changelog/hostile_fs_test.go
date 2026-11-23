package changelog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// appendOne records one update in the changelog of repoPath, so that the
// callers that build an index over the log have an entry to read.
func appendOne(t *testing.T, repoPath string) {
	t.Helper()

	_, err := Append(repoPath, Meta{Operation: "test", Actor: "tester"}, []Update{{
		Reference: "refs/heads/main",
		OldOID:    zeroOID,
		NewOID:    oidA,
	}})
	require.NoError(t, err)
}

// TestTheChangelogReportsAnUnusableDirectory covers the calls that cannot
// create the changelog directory, because a file holds its place.
func TestTheChangelogReportsAnUnusableDirectory(t *testing.T) {
	repoPath := newRepoPath(t)
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, dirName), []byte("not a directory"), 0o644))

	_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/main", NewOID: oidA}})
	require.Error(t, err)

	_, err = LookupLand(repoPath, "refs/heads/main", oidA)
	require.Error(t, err)

	_, err = RecordLandIntents(repoPath, "refs/heads/main", "inv", []Land{{Original: oidA, Landed: oidB}})
	require.Error(t, err)

	require.Error(t, CompactLandIntents(repoPath))

	_, err = LastSequence(repoPath)
	require.Error(t, err)

	_, err = List(repoPath, 0, 10)
	require.Error(t, err)
}

// TestTheChangelogReportsADirectoryWhereTheLogShouldBe covers the calls that
// open the log and find a directory in its place.
func TestTheChangelogReportsADirectoryWhereTheLogShouldBe(t *testing.T) {
	repoPath := newRepoPath(t)
	require.NoError(t, os.MkdirAll(Path(repoPath), 0o755))

	_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/main", NewOID: oidA}})
	require.Error(t, err)

	_, err = List(repoPath, 0, 10)
	require.Error(t, err)

	_, err = LastSequence(repoPath)
	require.Error(t, err)

	_, err = LookupLand(repoPath, "refs/heads/main", oidA)
	require.Error(t, err)

}

// TestTheLandIndexReportsAnUnusableFile covers the index of the lands whose
// file is a directory in place of a file, once the log holds an entry.
func TestTheLandIndexReportsAnUnusableFile(t *testing.T) {
	repoPath := newRepoPath(t)
	appendOne(t, repoPath)
	require.NoError(t, os.MkdirAll(landsPath(repoPath), 0o755))

	_, err := LookupLand(repoPath, "refs/heads/main", oidA)
	require.Error(t, err)
}

// TestTheLandIntentsReportAnUnusableFile covers the record of the intents
// whose file is a directory in place of a file.
func TestTheLandIntentsReportAnUnusableFile(t *testing.T) {
	repoPath := newRepoPath(t)
	require.NoError(t, os.MkdirAll(intentsPath(repoPath), 0o755))

	_, err := RecordLandIntents(repoPath, "refs/heads/main", "inv", []Land{{Original: oidA, Landed: oidB}})
	require.Error(t, err)
}

// TestTheChangelogReportsAFullDevice covers the writes that the file system
// refuses after the file was opened, and the reads that fail on the way.
func TestTheChangelogReportsAFullDevice(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("/dev/full is not available on this system")
	}

	t.Run("the log", func(t *testing.T) {
		repoPath := newRepoPath(t)
		require.NoError(t, os.MkdirAll(filepath.Join(repoPath, dirName), 0o755))
		require.NoError(t, os.Symlink("/dev/full", Path(repoPath)))

		_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/main", NewOID: oidA}})
		require.Error(t, err)
	})

	t.Run("the land intents", func(t *testing.T) {
		repoPath := newRepoPath(t)
		require.NoError(t, os.MkdirAll(filepath.Join(repoPath, dirName), 0o755))
		require.NoError(t, os.Symlink("/dev/full", intentsPath(repoPath)))

		_, err := RecordLandIntents(repoPath, "refs/heads/main", "inv", []Land{{Original: oidA, Landed: oidB}})
		require.Error(t, err)
	})
}
