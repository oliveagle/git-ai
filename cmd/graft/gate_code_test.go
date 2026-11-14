package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
)

// startGateServer starts a standalone server with the agent alice, a policy
// that lets only alice update main and her own temporary branches, and a rule
// on main that takes only a verified result.
func startGateServer(t *testing.T) serverEnv {
	t.Helper()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	cfg.Graft.Agents = []config.GraftAgent{{Name: "alice", Token: "alice-secret"}}
	cfg.Graft.Policy = []config.GraftPolicyRule{
		{Actor: "alice", Refs: []string{"refs/heads/main", "refs/heads/graft/alice/**"}, Allow: true},
		{Actor: "*", Allow: false},
	}
	cfg.Graft.Branches = []config.GraftBranchRule{{Name: "main", RequireVerifiedResult: true}}
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))))
	cfg.SocketPath = addr

	// The repository is created on disk because every RPC of the server needs the
	// agent token, and the test helper's creation path does not present one.
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	env := serverEnv{
		ctx:      ctx,
		cfg:      cfg,
		addr:     addr,
		storage:  cfg.Storages[0].Name,
		relPath:  repoProto.GetRelativePath(),
		repoPath: repoPath,
	}
	env.seedMain(t, map[string]string{"top.txt": "top\n"})
	return env
}

// TestLandPrintsTheGateCode checks that a land refused by the branch
// management policy prints the code of the decision: a CI branches on it, and
// the reason of the message is for a human.
//
// The land is made by an agent, because the gate binds the callers that
// authenticated with an agent token.
func TestLandPrintsTheGateCode(t *testing.T) {
	env := startGateServer(t)

	dir := filepath.Join(t.TempDir(), "ws")
	flags := []string{"-token", "alice-secret", "-storage", env.storage, "-user", "alice"}
	code, _, stderr := runCLI(t, append([]string{"clone", env.addr, env.relPath, dir}, flags...)...)
	require.Equal(t, exitOK, code, stderr)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "change.txt"), []byte("change\n"), 0o644))
	code, _, stderr = runCLI(t, append([]string{"commit", "-C", dir, "-author", "Dev <dev@example.com>", "-m", "add change"}, flags...)...)
	require.Equal(t, exitOK, code, stderr)

	// The branch has no checkpoint: the policy refuses the land.
	code, _, stderr = runCLI(t, append([]string{"land", "-C", dir, "--timestamp", "2026-11-14T00:00:00Z"}, flags...)...)
	require.Equal(t, exitError, code, stderr)
	require.Contains(t, stderr, "the branch policy refused the update: not_verified")
	require.Equal(t, env.seedMain(t, map[string]string{"top.txt": "top\n"}).String(),
		gittest.ResolveRevision(t, env.cfg, env.repoPath, "refs/heads/main").String(),
		"a refused land writes nothing")
}
