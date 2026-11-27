package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestTheImportIsReadFromTheEnvironment(t *testing.T) {
	t.Setenv("GRAFT_WORKER_IMPORT_SCRIPT", "/opt/git-ai/deploy/host-scripts/import-remote.sh")
	t.Setenv("GRAFT_WORKER_IMPORT_REPO", "quant_mt5.git")
	t.Setenv("GRAFT_WORKER_IMPORT_REMOTE", "ssh://git@ssh.github.com:443/oliveagle/quant_mt5.git")
	t.Setenv("GRAFT_WORKER_IMPORT_EVERY", "45m")
	t.Setenv("GRAFT_WORKER_IMPORT_TIMEOUT", "20m")

	o, err := parseOptions([]string{"worker"})
	require.NoError(t, err)

	importOptions := o.jobs().Import
	require.Equal(t, "/opt/git-ai/deploy/host-scripts/import-remote.sh", importOptions.Script)
	require.Equal(t, "quant_mt5.git", importOptions.Repository)
	require.Equal(t, "ssh://git@ssh.github.com:443/oliveagle/quant_mt5.git", importOptions.Remote)
	require.Equal(t, 45*time.Minute, importOptions.Every)
	require.Equal(t, 20*time.Minute, importOptions.Timeout)
}

func TestTheImportKeepsItsDefaultWhenTheEnvironmentIsEmpty(t *testing.T) {
	o, err := parseOptions([]string{"worker"})
	require.NoError(t, err)

	importOptions := o.jobs().Import
	require.Equal(t, "", importOptions.Script)
	require.Equal(t, 30*time.Minute, importOptions.Every)
	require.Equal(t, 30*time.Minute, importOptions.Timeout)
}
