package sdk

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
)

// TestReferenceValueAndUpdate checks the contract of the two reference
// methods of the SDK: a reference is read by its exact name, and it is moved
// with a compare-and-swap.
func TestReferenceValueAndUpdate(t *testing.T) {
	ctx := testhelper.Context(t)
	repo, repoPath, cfg := newOperatorRepo(t)

	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch("main"),
		gittest.WithTreeEntries(gittest.TreeEntry{Mode: "100644", Path: "main.go", Content: "package main\n"}),
	)

	// A reference that does not exist reads as empty, and not as an error.
	value, err := repo.ReferenceValue(ctx, "refs/graft/green/main")
	require.NoError(t, err)
	require.Empty(t, value)

	// The reference is moved, and reads back.
	require.NoError(t, repo.UpdateReference(ctx, "refs/graft/green/main", "", base.String()))

	value, err = repo.ReferenceValue(ctx, "refs/graft/green/main")
	require.NoError(t, err)
	require.Equal(t, base.String(), value)

	// A move whose old value is stale fails: a concurrent move is not
	// overwritten.
	err = repo.UpdateReference(ctx, "refs/graft/green/main", missingSpecOID, base.String())
	require.Error(t, err)

	value, err = repo.ReferenceValue(ctx, "refs/graft/green/main")
	require.NoError(t, err)
	require.Equal(t, base.String(), value)

	// A name that is not a full reference name is refused.
	_, err = repo.ReferenceValue(ctx, "main")
	require.Equal(t, codes.InvalidArgument, Code(err))

	err = repo.UpdateReference(ctx, "main", "", base.String())
	require.Equal(t, codes.InvalidArgument, Code(err))
}

// missingSpecOID is a well-formed object ID that does not exist in the test
// repositories.
const missingSpecOID = "0123456789abcdef0123456789abcdef01234567"

// newOperatorRepo creates a repository on a server that requires the shared
// token, and returns the repository, its path and the configuration of the
// server. The references the server reserves for itself (refs/graft/) need
// the operator credential: an agent may not move them.
func newOperatorRepo(t *testing.T) (*Repo, string, config.Cfg) {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = "spec-shared-secret"
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll)
	cfg.SocketPath = addr

	setupCtx := testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))
	repoProto, repoPath := gittest.CreateRepository(t, setupCtx, cfg)

	client, err := Dial(ctx, addr, WithStorage(cfg.Storages[0].Name), WithToken(cfg.Auth.Token))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	return client.Repo(repoProto.GetRelativePath()), repoPath, cfg
}
