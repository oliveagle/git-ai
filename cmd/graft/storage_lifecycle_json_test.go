package main

import (
	"encoding/json"
	"testing"

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

// TestCLIStorageLifecycleJSON covers the JSON output of the archive lifecycle:
// archive, unarchive and the admin-only start of the cleanup. It runs serially
// because the CLI tests share the package-level backend factory.
func TestCLIStorageLifecycleJSON(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	testcfg.RequireCredential(&cfg)
	cfg.Graft.AdminToken = "admin-secret"
	// The cleanup may start as soon as the repository is archived, which a real
	// deployment waits months for.
	cfg.Graft.Storage.ArchiveRetention = "1ns"
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

	code, stdout, stderr := cli("archive", "--json")
	require.Equal(t, exitOK, code, stderr)
	var archived sdk.StorageArchive
	require.NoError(t, json.Unmarshal([]byte(stdout), &archived))
	require.NotEmpty(t, archived.ArchivedBy)
	require.False(t, archived.ArchivedAt.IsZero())

	code, stdout, stderr = cli("unarchive", "--json")
	require.Equal(t, exitOK, code, stderr)
	var unarchived map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &unarchived))

	// The cleanup is the admin-only step: it closes the reads of an archived
	// repository and starts the observation period.
	code, _, stderr = cli("archive")
	require.Equal(t, exitOK, code, stderr)

	code, stdout, stderr = runCLI(t, append([]string{
		"storage", "cleanup-start", "--json", "-admin-token", "admin-secret",
	}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	var cleanup sdk.StorageCleanup
	require.NoError(t, json.Unmarshal([]byte(stdout), &cleanup))
	require.Equal(t, "admin", cleanup.ReadClosedBy)
	require.False(t, cleanup.PurgeAfter.Before(cleanup.ReadClosedAt))

	code, stdout, stderr = runCLI(t, append([]string{"storage", "archived", "--json"}, target[:4]...)...)
	require.Equal(t, exitOK, code, stderr)
	var listed []sdk.StorageArchivedRepository
	require.NoError(t, json.Unmarshal([]byte(stdout), &listed))
	require.Len(t, listed, 1)
	require.False(t, listed[0].ReadClosedAt.IsZero())
}
