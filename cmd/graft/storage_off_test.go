package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
)

// TestCLIStorageReportsACoreThatIsOff drives every subcommand against a server
// whose storage core is off: each one answers with the refusal of the core
// instead of an empty result that reads like success.
func TestCLIStorageReportsACoreThatIsOff(t *testing.T) {
	env := startServer(t)
	server := []string{"-address", env.addr, "-storage", env.storage}
	target := append(append([]string{}, server...), "-repo", env.relPath)
	cli := func(args ...string) (int, string, string) {
		return runCLI(t, append(append([]string{"storage"}, args...), target...)...)
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "verify", args: []string{"verify"}},
		{name: "checkpoint", args: []string{"checkpoint"}},
		{name: "archive", args: []string{"archive"}},
		{name: "unarchive", args: []string{"unarchive"}},
		{name: "gc", args: []string{"gc"}},
		{name: "gc removed", args: nil},
		{name: "archived", args: nil},
		{name: "cleanup", args: []string{"cleanup-start", "-admin-token", "anything"}},
		{name: "purge", args: []string{"purge", "-admin-token", "anything"}},
		{name: "delete conflicting ref", args: []string{"delete-conflicting-ref", "refs/heads/main", "aaa"}},
	} {
		var err error
		code := exitOK
		var stdout, stderr string
		switch tc.name {
		case "gc removed":
			code, stdout, stderr = runCLI(t, append(append([]string{"storage"}, "gc", "--removed"), server...)...)
		case "archived":
			code, stdout, stderr = runCLI(t, append(append([]string{"storage"}, "archived"), server...)...)
		default:
			code, stdout, stderr = cli(tc.args...)
		}
		_ = err
		require.Equal(t, exitError, code, "%s: stdout=%q stderr=%q", tc.name, stdout, stderr)
		require.NotEmpty(t, stderr, "%s", tc.name)
	}
}

// TestCLIStorageGCAndCheckpointEdges covers the dry run of gc as its text
// prints it, and the checkpoint of a repository that has nothing to
// consolidate.
func TestCLIStorageGCAndCheckpointEdges(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	testcfg.RequireCredential(&cfg)
	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative})
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, testserver.WithGraftStorage(core))

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	target := []string{"-address", addr, "-storage", repo.GetStorageName(), "-repo", repo.GetRelativePath()}
	cli := func(args ...string) (int, string, string) {
		return runCLI(t, append(append([]string{"storage"}, args...), target...)...)
	}

	code, _, stderr := cli("import")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := cli("checkpoint")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "checkpoint created")

	// A checkpoint at the head exists already, and the command says so
	// instead of creating it again.
	code, stdout, stderr = cli("checkpoint")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "exists already")

	// The dry run of the collector says what it would delete.
	code, stdout, stderr = cli("gc", "--dry-run")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "would delete")
}

// TestStorageJSONReportsAFailingWriter covers the JSON output of the storage
// commands: a writer that fails is an error, not a silent success.
func TestStorageJSONReportsAFailingWriter(t *testing.T) {
	env := startServer(t)
	target := []string{"-address", env.addr, "-storage", env.storage, "-repo", env.relPath}
	for _, args := range [][]string{
		{"storage", "checkpoint", "--json"},
		{"storage", "state", "--json"},
		{"storage", "verify", "--json"},
		{"storage", "import", "--json"},
		{"storage", "archive", "--json"},
	} {
		var stderr bytes.Buffer
		code := run(append(args, target...), failingWriter{}, &stderr)
		require.NotEqual(t, exitOK, code, "%v: %s", args, stderr.String())
	}
}
