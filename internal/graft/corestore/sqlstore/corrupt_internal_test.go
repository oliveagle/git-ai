package sqlstore

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
)

// TestTheStoreReportsTheRowsItCannotDecode covers the reads of a row that the
// database holds and the store cannot make sense of: the entry is not JSON, the
// time is not a time. A broken row is reported with the table it came from, so
// that an operator knows what to repair.
func TestTheStoreReportsTheRowsItCannotDecode(t *testing.T) {
	store, ctx := newFaultStore(t)
	repo := store.register(t, ctx)
	repoID := repo.ID
	key := mirrorstate.Key{Storage: "default", RelativePath: "@hashed/x.git"}

	t.Run("a changelog entry", func(t *testing.T) {
		_, err := store.db.ExecContext(ctx, "INSERT INTO graft_changelog (repo_id, log_seq, entry) VALUES (?, 1, 'not json')", repoID)
		require.NoError(t, err)
		_, err = store.ScanChangelog(ctx, repoID, 0, 0)
		require.ErrorContains(t, err, "decode changelog entry")
	})

	t.Run("a review body", func(t *testing.T) {
		_, err := store.db.ExecContext(ctx,
			"INSERT INTO graft_reviews (repo_id, review_id, version, target_branch, closed, body, updated_at) VALUES (?, 1, 1, ?, 0, 'not json', '2026-01-02T03:04:05Z')",
			repoID, []byte("main"))
		require.NoError(t, err)
		_, err = store.ListReviews(ctx, repoID, "main", true, 0)
		require.ErrorContains(t, err, "decode review")
	})

	t.Run("the branches of a mirror target", func(t *testing.T) {
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO graft_mirror_targets (storage, relative_path, name, url, branches, enabled, created_at, updated_at)
			VALUES (?, ?, 'gitee', 'https://gitee.com/x/y.git', 'not json', 1, '2026-01-02T03:04:05Z', '2026-01-02T03:04:05Z')`,
			key.Storage, key.RelativePath)
		require.NoError(t, err)
		_, err = store.ListTargets(ctx, key)
		require.ErrorContains(t, err, "decode the branches of the mirror target")
	})

	t.Run("the time of a mirror target", func(t *testing.T) {
		_, err := store.db.ExecContext(ctx, `
			UPDATE graft_mirror_targets SET branches = '[]', created_at = 'not a time'
			WHERE storage = ? AND relative_path = ? AND name = 'gitee'`, key.Storage, key.RelativePath)
		require.NoError(t, err)
		_, err = store.ListTargets(ctx, key)
		require.ErrorContains(t, err, "read the creation time of the mirror target")
	})

	t.Run("the branches of a mirror synchronisation", func(t *testing.T) {
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO graft_mirror_syncs (storage, relative_path, name, status, branches, published_at, error)
			VALUES (?, ?, 'gitee', 'ok', 'not json', '2026-01-02T03:04:05Z', '')`,
			key.Storage, key.RelativePath)
		require.NoError(t, err)
		_, err = store.ListSyncs(ctx, key)
		require.ErrorContains(t, err, "decode the branches of the mirror synchronisation")
	})

	t.Run("the time of a mirror synchronisation", func(t *testing.T) {
		_, err := store.db.ExecContext(ctx, `
			UPDATE graft_mirror_syncs SET branches = '{}', published_at = 'not a time'
			WHERE storage = ? AND relative_path = ? AND name = 'gitee'`, key.Storage, key.RelativePath)
		require.NoError(t, err)
		_, err = store.ListSyncs(ctx, key)
		require.ErrorContains(t, err, "read the publication time of the mirror synchronisation")
	})
}

// TestTheStoreRefusesAnUnreadableRepositoryIdentifier covers the identifier a
// caller passes: a repository the database does not hold is NotFound, and the
// reference methods report it instead of answering an empty map.
func TestTheStoreRefusesAnUnreadableRepositoryIdentifier(t *testing.T) {
	store, ctx := newFaultStore(t)
	store.register(t, ctx)

	_, _, err := store.Refs(ctx, 999999)
	require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
}
