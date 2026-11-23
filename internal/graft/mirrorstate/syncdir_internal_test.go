package mirrorstate

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSyncDirReportsAMissingDirectory checks that the fsync of the directory of
// a rename reports a directory that is not there, instead of hiding it.
func TestSyncDirReportsAMissingDirectory(t *testing.T) {
	require.Error(t, syncDir(filepath.Join(t.TempDir(), "gone")))
}

// TestSyncDirReportsADirectoryThatCannotBeSynced checks that a directory whose
// fsync fails is reported: reporting success would let a caller believe the
// rename is durable.
func TestSyncDirReportsADirectoryThatCannotBeSynced(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("a file system that refuses the fsync of a directory is needed")
	}

	require.Error(t, syncDir("/proc/self"))
}
