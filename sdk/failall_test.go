package sdk_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/sdk"
)

// refuseEveryCall makes the server answer every RPC with a refusal that is not
// transient, so that the SDK reports it instead of retrying.
func refuseEveryCall(t *testing.T) *sdk.Client {
	t.Helper()

	// A server with no service registered answers every RPC with
	// Unimplemented, which is the refusal of the whole surface at once.
	listener := startServer(t, func(*grpc.Server) {})
	client, err := sdk.Dial(t.Context(), listener)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestEveryCallReportsAServerThatRefusesIt covers the error path of every
// read and write of the SDK: a call the server refuses is returned to the
// caller, never read as an empty answer.
func TestEveryCallReportsAServerThatRefusesIt(t *testing.T) {
	ctx := context.Background()
	client := refuseEveryCall(t)
	repo := client.Repo("project.git")

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"head", func() error { _, err := repo.Head(ctx); return err }},
		{"has references", func() error { _, err := repo.HasReferences(ctx); return err }},
		{"reference exists", func() error { _, err := repo.ReferenceExists(ctx, "refs/heads/main"); return err }},
		{"zero oid", func() error { _, err := repo.ZeroOID(ctx); return err }},
		{"resolve revision", func() error { _, err := repo.ResolveRevision(ctx, "main"); return err }},
		{"read file", func() error { _, err := repo.ReadFile(ctx, "main", "a.txt"); return err }},
		{"list tree", func() error { _, err := repo.ListTree(ctx, "main", "", false); return err }},
		{"tree id", func() error { _, err := repo.TreeID(ctx, "main"); return err }},
		{"reference value", func() error { _, err := repo.ReferenceValue(ctx, "refs/heads/main"); return err }},
		{"update reference", func() error { return repo.UpdateReference(ctx, "refs/heads/a", "", "b") }},
		{"update references", func() error {
			return repo.UpdateReferences(ctx, []sdk.ReferenceUpdate{{Name: "refs/heads/a", New: "b"}})
		}},
		{"create branch", func() error {
			return repo.CreateBranch(ctx, sdk.CreateBranchOptions{Branch: "b", StartPoint: "c"})
		}},
		{"changesets", func() error { _, err := repo.Changesets(ctx, 0, 10); return err }},
		{"blame", func() error { _, err := repo.Blame(ctx, "main", "a.txt"); return err }},
		{"path history", func() error { _, err := repo.PathHistory(ctx, "main", "a.txt", 10); return err }},
		{"get objects", func() error { _, err := repo.GetObjects(ctx, []string{"a"}); return err }},
		{"branch rules", func() error { _, err := repo.BranchRules(ctx); return err }},
		{"branch decision", func() error {
			_, err := repo.BranchDecision(ctx, sdk.BranchDecisionOptions{Branch: "main"})
			return err
		}},
		{"list reviews", func() error { _, err := repo.ListReviews(ctx, sdk.ListReviewsOptions{}); return err }},
		{"get review", func() error { _, err := repo.GetReview(ctx, "1"); return err }},
		{"search", func() error { _, _, _, err := repo.SearchCode(ctx, sdk.SearchOptions{Query: "x"}); return err }},
		{"storage import", func() error { _, err := repo.StorageImport(ctx); return err }},
		{"storage state", func() error { _, err := repo.StorageState(ctx); return err }},
		{"storage verify", func() error { _, err := repo.StorageVerify(ctx, false); return err }},
		{"storage checkpoint", func() error { _, err := repo.StorageCheckpoint(ctx); return err }},
		{"storage delete conflicting ref", func() error {
			_, err := repo.StorageDeleteConflictingRef(ctx, "refs/heads/a", "b")
			return err
		}},
		{"storage gc", func() error { _, err := repo.StorageGC(ctx, false); return err }},
		{"storage gc removed", func() error { _, err := client.StorageGCRemoved(ctx, "default", false); return err }},
		{"storage archive", func() error { _, err := repo.StorageArchive(ctx); return err }},
		{"storage start cleanup", func() error { _, err := repo.StorageStartCleanup(ctx); return err }},
		{"storage unarchive", func() error { return repo.StorageUnarchive(ctx) }},
		{"storage archived", func() error { _, err := client.StorageArchived(ctx, "default"); return err }},
		{"storage purge", func() error { _, err := repo.StoragePurge(ctx); return err }},
		{"list mirror targets", func() error { _, _, _, err := repo.ListMirrorTargets(ctx, "main"); return err }},
		{"unregister mirror target", func() error { return repo.UnregisterMirrorTarget(ctx, "m") }},
		{"register mirror target", func() error {
			_, err := repo.RegisterMirrorTarget(ctx, sdk.MirrorTargetOptions{
				Name: "m", URL: "https://example.com/x.git", Branches: []string{"main"},
			})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.Error(t, err)
			require.NotEqual(t, codes.OK, status.Code(err))
		})
	}
}

// TestCreateReportsAServerThatRefusesIt covers the creation of a repository,
// and the branch the server already holds.
func TestCreateReportsAServerThatRefusesIt(t *testing.T) {
	client := refuseEveryCall(t)
	err := client.Repo("project.git").Create(context.Background(), sdk.CreateOptions{})
	require.Error(t, err)
}
