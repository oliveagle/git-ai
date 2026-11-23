package sdk_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"

	"github.com/feder-cr/git-ai/sdk"
)

// TestCommitFilesRefusesEveryActionItCannotApply covers the actions the SDK
// refuses before it contacts the server, in every shape a caller may hold
// them in.
func TestCommitFilesRefusesEveryActionItCannotApply(t *testing.T) {
	t.Parallel()

	repo := dialFake(t, newFakeServices().register).Repo("repo")

	for _, tc := range []struct {
		name    string
		actions []sdk.Action
	}{
		{name: "a delete without a path", actions: []sdk.Action{sdk.Delete{}}},
		{name: "a put by pointer", actions: []sdk.Action{&sdk.Put{}}},
		{name: "a delete by pointer", actions: []sdk.Action{&sdk.Delete{}}},
		{name: "a move by pointer", actions: []sdk.Action{&sdk.Move{From: "a"}}},
		{name: "a chmod by pointer", actions: []sdk.Action{&sdk.Chmod{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := validCommit()
			opts.Actions = tc.actions

			_, err := repo.CommitFiles(t.Context(), opts)
			require.Equal(t, codes.InvalidArgument, sdk.Code(err))
		})
	}
}

// fakeRepositories answers the calls the SDK makes to the repository service.
type fakeRepositories struct {
	gitalypb.UnimplementedRepositoryServiceServer
}

func (f *fakeRepositories) ObjectFormat(context.Context, *gitalypb.ObjectFormatRequest) (*gitalypb.ObjectFormatResponse, error) {
	return &gitalypb.ObjectFormatResponse{Format: gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1}, nil
}

// TestTheReferenceCallsRefuseNamesBelowRefs covers the names the reference
// calls refuse before they contact the server.
func TestTheReferenceCallsRefuseNamesBelowRefs(t *testing.T) {
	t.Parallel()

	repo := dialFake(t, func(srv *grpc.Server) {
		newFakeServices().register(srv)
		gitalypb.RegisterRepositoryServiceServer(srv, &fakeRepositories{})
	}).Repo("repo")

	_, err := repo.ReferenceValue(t.Context(), "main")
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))

	_, err = repo.ReferenceExists(t.Context(), "main")
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))

	err = repo.UpdateReference(t.Context(), "main", "old", "new")
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))

	err = repo.UpdateReference(t.Context(), "refs/heads/main", "old", "")
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))

	err = repo.UpdateReferences(t.Context(), nil)
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))

	err = repo.UpdateReferences(t.Context(), []sdk.ReferenceUpdate{{Name: "main", New: "new"}})
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))

	err = repo.UpdateReferences(t.Context(), []sdk.ReferenceUpdate{{Name: "refs/heads/main"}})
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))
}

// TestTheCallsWithoutAServiceFail covers the calls that reach a server that
// does not have the service they need.
func TestTheCallsWithoutAServiceFail(t *testing.T) {
	t.Parallel()

	client := dialFake(t, func(*grpc.Server) {})
	repo := client.Repo("repo")

	_, err := repo.Head(t.Context())
	require.Error(t, err)

	_, err = repo.HasReferences(t.Context())
	require.Error(t, err)

	_, err = repo.ZeroOID(t.Context())
	require.Error(t, err)

	_, err = repo.Changesets(t.Context(), 0, 10)
	require.Error(t, err)

	err = repo.CreateBranch(t.Context(), sdk.CreateBranchOptions{Branch: "main", StartPoint: "abc"})
	require.Error(t, err)

	_, err = repo.RegisterMirrorTarget(t.Context(), sdk.MirrorTargetOptions{
		Name:     "github",
		URL:      "ssh://git@ssh.github.com:443/oliveagle/repo.git",
		Branches: []string{"main"},
	})
	require.Error(t, err)

	_, err = repo.Mirror(t.Context(), sdk.MirrorOptions{URL: "ssh://git@ssh.github.com:443/oliveagle/repo.git"})
	require.Error(t, err)
}

// TestTheCallsWithoutAValueFail covers the calls that refuse their own
// arguments before they contact the server.
func TestTheCallsWithoutAValueFail(t *testing.T) {
	t.Parallel()

	repo := dialFake(t, newFakeServices().register).Repo("repo")

	err := repo.CreateBranch(t.Context(), sdk.CreateBranchOptions{})
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))

	err = repo.CreateBranch(t.Context(), sdk.CreateBranchOptions{Branch: "main"})
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))

	err = repo.CreateBranch(t.Context(), sdk.CreateBranchOptions{StartPoint: "abc"})
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))
}

// TestAgentSignerReportsTheKeyItCannotRead covers the private key of an agent
// that the SDK cannot load.
func TestAgentSignerReportsTheKeyItCannotRead(t *testing.T) {
	t.Parallel()

	_, err := sdk.AgentSigner(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)

	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte("not a key"), 0o600))
	_, err = sdk.AgentSigner(path)
	require.Error(t, err)
}
