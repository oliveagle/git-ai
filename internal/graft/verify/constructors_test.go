package verify

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestOperationScratchRefusesAnIncompleteRequest covers the request of a
// scratch revert that cannot be served: the reverter is not wired, the base is
// missing, or nobody is named to commit the revert as.
func TestOperationScratchRefusesAnIncompleteRequest(t *testing.T) {
	_, _, err := (&OperationScratch{}).RevertOnto(context.Background(), "base", []string{"c"})
	require.ErrorContains(t, err, "scratch reverter is not configured")

	s := &OperationScratch{
		Operations: gitalypb.NewOperationServiceClient(nil),
		Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "x.git"},
	}
	_, _, err = s.RevertOnto(context.Background(), "", []string{"c"})
	require.ErrorContains(t, err, "scratch revert needs a base commit")

	_, _, err = s.RevertOnto(context.Background(), "base", []string{"c"})
	require.ErrorContains(t, err, "needs a committer name and email")
}

// TestNewBisectorRefusesAnIncompleteSetup covers the bisector a wrong setup
// asks for, and the bisector of a verifier that reverts nothing: it is built
// without a reverter.
func TestNewBisectorRefusesAnIncompleteSetup(t *testing.T) {
	_, err := NewBisector(Config{}, nil, nil, nil)
	require.ErrorContains(t, err, "bisector needs a connection")

	conn, err := grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	bisector, err := NewBisector(Config{}, conn, &gitalypb.Repository{StorageName: "default", RelativePath: "x.git"}, workspace.NewMemoryBackend())
	require.NoError(t, err)
	require.NotNil(t, bisector)
	require.Nil(t, bisector.Reverts, "a verifier that reverts nothing has no reverter")
}
