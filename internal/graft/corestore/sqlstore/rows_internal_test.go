package sqlstore

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestTheStoreReportsRowsThatBreakPartWay covers the loops of the store that
// read a result set: a database that fails while the rows are read is
// reported, never taken for an empty or a complete answer.
func TestTheStoreReportsRowsThatBreakPartWay(t *testing.T) {
	t.Run("the mirror targets", func(t *testing.T) {
		store, ctx := newFaultStore(t)
		store.register(t, ctx)
		key := mirrorstate.Key{Storage: "default", RelativePath: "@hashed/x.git"}
		require.NoError(t, store.RegisterTarget(ctx, key, faultMirrorTarget()))
		require.NoError(t, store.RegisterTarget(ctx, key, mirrorstate.Target{
			Name: "gitlab", URL: "https://gitlab.invalid/x.git", Branches: []string{"main"}, Enabled: true,
		}))

		store.faultRows(t, "graft_mirror_targets")
		_, err := store.ListTargets(ctx, key)
		require.ErrorContains(t, err, "list the mirror targets")
	})

	t.Run("the mirror synchronisations", func(t *testing.T) {
		store, ctx := newFaultStore(t)
		store.register(t, ctx)
		key := mirrorstate.Key{Storage: "default", RelativePath: "@hashed/x.git"}
		require.NoError(t, store.RecordSync(ctx, key, faultMirrorSync()))
		sync := faultMirrorSync()
		sync.Target = "gitlab"
		require.NoError(t, store.RecordSync(ctx, key, sync))

		store.faultRows(t, "graft_mirror_syncs")
		_, err := store.ListSyncs(ctx, key)
		require.ErrorContains(t, err, "list the mirror synchronisations")
	})

	t.Run("the reviews", func(t *testing.T) {
		store, ctx := newFaultStore(t)
		repo := store.register(t, ctx)
		for range 2 {
			_, err := store.InsertReview(ctx, repo.ID, func(id string) (*gitalypb.Review, error) {
				return &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t"), HeadCommitId: "abc"}, nil
			}, corestore.ReviewInsert{})
			require.NoError(t, err)
		}

		store.faultRows(t, "FROM graft_reviews")
		_, err := store.ListReviews(ctx, repo.ID, "", true, 0)
		require.ErrorContains(t, err, "list reviews")
	})

	t.Run("the packs of the garbage collector", func(t *testing.T) {
		store, ctx := newFaultStore(t)
		repo := store.register(t, ctx)
		for seq := range 2 {
			pack := corestore.NewPackKey(repo.ID, sha256.Sum256([]byte{byte(seq)}))
			_, err := store.db.ExecContext(ctx, `
				INSERT INTO graft_changesets (repo_id, seq, operation, actor, node_id, pack_key, created_at)
				VALUES (?, ?, 'test', 'tester', 'node-a', ?, '2026-01-02T03:04:05Z')`,
				repo.ID, seq+1, pack)
			require.NoError(t, err)
		}

		store.faultRows(t, "FROM graft_changesets")
		_, err := store.GCSnapshot(ctx, repo.ID)
		require.ErrorContains(t, err, "read changeset packs")
	})
}
