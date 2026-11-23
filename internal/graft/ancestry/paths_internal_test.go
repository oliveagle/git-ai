package ancestry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCheckHistoryWalkedAsStoredReportsUnreadablePaths covers the checks that
// cannot read what they look for: a path of the wrong shape is an error, not
// an absent file, because a repository whose history cannot be inspected is
// one the graph index must not answer for.
func TestCheckHistoryWalkedAsStoredReportsUnreadablePaths(t *testing.T) {
	t.Parallel()

	// refs is a file, so refs/replace cannot be listed.
	notADir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(notADir, "refs"), []byte("x"), 0o600))
	err := checkHistoryWalkedAsStored(notADir)
	require.ErrorContains(t, err, "reading replace refs")

	// packed-refs is a directory, so it cannot be read.
	unreadable := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(unreadable, "packed-refs"), 0o755))
	err = checkHistoryWalkedAsStored(unreadable)
	require.ErrorContains(t, err, "reading packed refs")

	// The shallow check names the path it could not inspect.
	file := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.WriteFile(file, []byte("not a repository"), 0o600))
	err = checkHistoryWalkedAsStored(file)
	require.ErrorContains(t, err, "checking shallow")
}

// TestCheckHistoryWalkedAsStoredRefusesReplaceRefs covers the two places a
// replace ref hides: the loose refs under refs/replace and the packed-refs
// file. Either one means git walks a history the object database does not
// describe, so the repository is refused.
func TestCheckHistoryWalkedAsStoredRefusesReplaceRefs(t *testing.T) {
	t.Parallel()

	loose := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(loose, "refs", "replace"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(loose, "refs", "replace", "abc"), []byte("0123"), 0o600))
	err := checkHistoryWalkedAsStored(loose)
	require.ErrorContains(t, err, "repository has replace refs")

	packed := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(packed, "packed-refs"),
		[]byte("# pack-refs with: peeled fully-peeled sorted\n0123 refs/replace/abc\n"), 0o600))
	err = checkHistoryWalkedAsStored(packed)
	require.ErrorContains(t, err, "repository has replace refs")
}
