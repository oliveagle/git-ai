//go:build unix

package workspace

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStatusIgnoresSpecialFiles checks that a named pipe in the workspace is
// neither a change nor an error: reading it would block. The test lives in a
// file of its own because mkfifo has no Windows spelling, and the workspace is
// also built and tested there.
func TestStatusIgnoresSpecialFiles(t *testing.T) {
	t.Parallel()

	b := seedBackend(t, []string{"a.txt"}, []string{"a"})
	ws := cloneTestWorkspace(t, b)

	require.NoError(t, syscall.Mkfifo(filepath.Join(ws.Root(), "pipe"), 0o644))

	changes, err := ws.Status()
	require.NoError(t, err)
	require.Empty(t, changes)
}
