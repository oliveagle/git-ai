package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRunRejectsTheOptionsItCannotServe covers the flag handling of the daemon
// without starting one: every refusal happens before the listen.
func TestRunRejectsTheOptionsItCannotServe(t *testing.T) {
	t.Parallel()

	require.NoError(t, run([]string{"-h"}), "help is not a failure")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"missing listen":     {args: []string{"--upstream", "tcp://127.0.0.1:1"}, want: "--listen is required"},
		"missing upstream":   {args: []string{"--listen", "tcp://127.0.0.1:0"}, want: "--upstream is required"},
		"positional":         {args: []string{"--listen", "tcp://127.0.0.1:0", "--upstream", "tcp://127.0.0.1:1", "extra"}, want: "unexpected arguments"},
		"unknown flag":       {args: []string{"--bogus"}, want: "flag provided but not defined"},
		"chunk size":         {args: []string{"--listen", "tcp://127.0.0.1:0", "--upstream", "tcp://127.0.0.1:1", "--chunk-size", "0"}, want: "chunk-size must be positive"},
		"disk max bytes":     {args: []string{"--listen", "tcp://127.0.0.1:0", "--upstream", "tcp://127.0.0.1:1", "--disk-max-bytes", "0"}, want: "disk-max-bytes must be positive"},
		"invalid listen":     {args: []string{"--listen", "bogus", "--upstream", "tcp://127.0.0.1:1"}, want: "invalid --listen"},
		"invalid upstream":   {args: []string{"--listen", "tcp://127.0.0.1:0", "--upstream", "bogus://x"}, want: "dial upstream"},
		"unknown tier":       {args: []string{"--listen", "tcp://127.0.0.1:0", "--upstream", "tcp://127.0.0.1:1", "--tier", "bogus"}, want: "configure cache tier"},
		"disk dir in daemon": {args: []string{"--listen", "tcp://127.0.0.1:0", "--upstream", "tcp://127.0.0.1:1", "--tier", "daemon", "--disk-dir", "/tmp/x"}, want: "configure cache tier"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.ErrorContains(t, run(tc.args), tc.want)
		})
	}
}

// TestRunReportsTheListenItCannotTake covers the failures between a parsed
// configuration and a served listener.
func TestRunReportsTheListenItCannotTake(t *testing.T) {
	t.Parallel()

	// A seed that cannot be opened stops the start before the listener.
	require.ErrorContains(t, run([]string{"--listen", "tcp://127.0.0.1:0", "--upstream", "tcp://127.0.0.1:1",
		"--seed", filepath.Join(t.TempDir(), "missing.seed")}), "open seed")

	// A socket path whose directory does not exist
	err := run([]string{"--listen", "unix:" + t.TempDir() + "/missing/cache.sock", "--upstream", "tcp://127.0.0.1:1"})
	require.ErrorContains(t, err, "listen")

	// A path that is a directory cannot be replaced by a socket, and the stale
	// socket removal says so.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep"), []byte("x"), 0o600))
	err = run([]string{"--listen", "unix:" + dir, "--upstream", "tcp://127.0.0.1:1"})
	require.ErrorContains(t, err, "remove stale socket")
}

// TestRunSnapshotRejectsWhatItCannotSnapshot covers the snapshot entry point:
// the flag handling and the failures that happen before an upstream is used.
func TestRunSnapshotRejectsWhatItCannotSnapshot(t *testing.T) {
	t.Parallel()

	require.NoError(t, run([]string{"snapshot", "-h"}), "help is not a failure")
	require.ErrorContains(t, run([]string{"snapshot"}), "--upstream is required")
	require.ErrorContains(t, run([]string{
		"snapshot", "--upstream", "bogus://x", "--repo", "demo.git", "--rev", "main", "--out", "/tmp/x.seed",
	}), "dial upstream")

	// The seed file goes next to the output, which must be a directory that exists.
	require.ErrorContains(t, run([]string{
		"snapshot", "--upstream", "tcp://127.0.0.1:1", "--repo", "demo.git", "--rev", "main",
		"--out", filepath.Join(t.TempDir(), "missing", "x.seed"),
	}), "create seed file")

	// The snapshot reads the upstream: a refusal is reported with the revision
	// and the repository it was for.
	require.ErrorContains(t, run([]string{
		"snapshot", "--upstream", "tcp://127.0.0.1:1", "--repo", "demo.git", "--rev", "main",
		"--out", filepath.Join(t.TempDir(), "x.seed"),
	}), "snapshot main of demo.git")
}
