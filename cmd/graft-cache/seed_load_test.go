package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/feder-cr/git-ai/internal/graft/cachetier"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// newSeedingServer returns a cache tier whose upstream is never asked: loading a
// seed only reads the file.
func newSeedingServer(t *testing.T) *cachetier.Server {
	t.Helper()

	conn, err := grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	server, err := cachetier.NewServer(conn, cachetier.Config{Tier: cachetier.TierDaemon, MaxBytes: 1 << 20})
	require.NoError(t, err)
	return server
}

// writeSeedGzip writes the bytes of a seed file, compressed as a seed is.
func writeSeedGzip(t *testing.T, content string) string {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	path := filepath.Join(t.TempDir(), "cache.seed")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// TestLoadSeedFiles covers what preloading reports: a file that cannot be
// opened or is not a seed stops the start, a whole seed is logged, and a seed
// with a damaged tail is logged as damaged after the records it could read.
func TestLoadSeedFiles(t *testing.T) {
	t.Parallel()

	server := newSeedingServer(t)
	logger := testhelper.NewLogger(t)

	err := loadSeedFiles(server, []string{filepath.Join(t.TempDir(), "missing.seed")}, logger)
	require.ErrorContains(t, err, "open seed")

	notGzip := filepath.Join(t.TempDir(), "not.seed")
	require.NoError(t, os.WriteFile(notGzip, []byte("not a seed"), 0o600))
	err = loadSeedFiles(server, []string{notGzip}, logger)
	require.ErrorContains(t, err, "open seed")

	empty := writeSeedGzip(t, "graft-cache-seed v1\n")
	require.NoError(t, loadSeedFiles(server, []string{empty}, logger))

	// A frame that does not decode ends the seed and is reported, not returned.
	damaged := writeSeedGzip(t, "graft-cache-seed v1\n\xff\xff\xff\xff\xff\xff\xff\xff\xff\xff")
	require.NoError(t, loadSeedFiles(server, []string{damaged}, logger))
}
