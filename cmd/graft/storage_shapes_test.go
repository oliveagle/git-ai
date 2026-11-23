package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/sdk"
)

// TestCLIStorageReportsWhatItWouldDo covers the text the maintenance commands
// print for the shapes the JSON output cannot show: a checkpoint that exists
// already, a collection that would delete rather than delete, and an archived
// repository whose reads are closed and whose purge has a date.
func TestCLIStorageReportsWhatItWouldDo(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	testcfg.RequireCredential(&cfg)
	cfg.Graft.AdminToken = "admin-secret"
	// The test starts the cleanup at once; a real deployment keeps the archive
	// for months before an operator may begin it.
	cfg.Graft.Storage.ArchiveRetention = "1ns"
	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative})
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, testserver.WithGraftStorage(core))

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	server := []string{"-address", addr, "-storage", repo.GetStorageName()}
	target := append(append([]string{}, server...), "-repo", repo.GetRelativePath())
	cli := func(args ...string) (int, string, string) {
		return runCLI(t, append(append([]string{"storage"}, args...), target...)...)
	}

	code, _, stderr := cli("import")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr := cli("checkpoint")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "checkpoint created at changeset")

	// The second checkpoint at the same head is not created: the command says
	// what is there instead.
	code, stdout, stderr = cli("checkpoint")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "exists already")

	code, stdout, stderr = cli("gc", "--dry-run")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "would delete 0 objects")

	code, stdout, stderr = runCLI(t, append([]string{"storage", "gc", "--removed", "--dry-run"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "would delete 0 objects (0 bytes) of 0 removed repositories")

	// The JSON of a collection that changed nothing is still one object.
	code, stdout, stderr = runCLI(t, append([]string{"storage", "gc", "--removed", "--json"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	var collected sdk.StorageGCRemoved
	require.NoError(t, json.Unmarshal([]byte(stdout), &collected))

	code, stdout, stderr = cli("verify", "--json")
	require.Equal(t, exitOK, code, stderr)
	var verify sdk.StorageVerify
	require.NoError(t, json.Unmarshal([]byte(stdout), &verify))

	code, _, stderr = cli("archive")
	require.Equal(t, exitOK, code, stderr)

	// An archived repository whose reads are still open has no date.
	code, stdout, stderr = runCLI(t, append([]string{"storage", "archived"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "reads open")

	code, stdout, stderr = runCLI(t, append([]string{"storage", "archived", "--json"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	var archived []sdk.StorageArchivedRepository
	require.NoError(t, json.Unmarshal([]byte(stdout), &archived))
	require.Len(t, archived, 1)

	// Once cleanup has started, the same row reads as purgeable from a date.
	args := append([]string{"storage", "cleanup-start"}, append(target, "-admin-token", "admin-secret")...)
	code, _, stderr = runCLI(t, args...)
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, append([]string{"storage", "archived"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "read closed; purgeable from ")

	// The observation period of the deployment is days; the test ages it.
	require.NoError(t, setPurgeAfterForCLI(t, core, repo, time.Now().UTC().Add(-time.Minute)))
	code, stdout, stderr = runCLI(t, append(append([]string{"storage", "purge"}, target...), "-admin-token", "admin-secret")...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "purged: repository ID")
}
