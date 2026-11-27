package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheTokenIsReadFromItsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte("  the-shared-token\n"), 0o600))

	o := &options{tokenFile: path}
	require.Equal(t, "the-shared-token", o.token())

	// A credential on the command line wins over the file.
	o.graftToken = "from-the-line"
	require.Equal(t, "from-the-line", o.token())

	// A file that is not there is no credential.
	o.graftToken = ""
	o.tokenFile = filepath.Join(dir, "missing")
	require.Equal(t, "", o.token())
}
