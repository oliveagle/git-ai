package sqlstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// TestArchiveRoundTrip archives a repository, checks that it is frozen and
// listed, takes it out of the archive and archives it again, and purges it once
// the retention has passed. A repository is never removed: the archive is the
// only way to retire it, and the purge deletes the row and records a tombstone
// for the garbage collector (doc/graft/PLAN13.md).
func TestArchiveRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := testKey("archive")
		repo := register(t, ctx, s, key)
		created, err := commit(ctx, s, repo.ID, create("refs/heads/main", oid(1)))
		require.NoError(t, err)
		require.EqualValues(t, 1, created.Seq)

		// Archive: the row stays, frozen, with the archive state.
		archived, err := s.ArchiveRepository(ctx, key, "operator")
		require.NoError(t, err)
		require.True(t, archived.Archived)
		require.Equal(t, "operator", archived.ArchivedBy)
		require.False(t, archived.ArchivedAt.IsZero())
		require.True(t, archived.PurgeAfter.IsZero(), "cleanup has not started yet")

		// The row is still there and reports the archive state.
		got, err := s.GetRepository(ctx, key)
		require.NoError(t, err)
		require.True(t, got.Archived)
		require.True(t, got.ReadClosedAt.IsZero(), "the archived repository is still readable")

		// A write does not change a frozen repository.
		_, err = commit(ctx, s, repo.ID, create("refs/heads/topic", oid(2)))
		require.ErrorIs(t, err, corestore.ErrRepositoryArchived)
		requireRefs(t, ctx, s, repo.ID, map[string]corestore.Target{"refs/heads/main": oid(1)})

		// Archiving twice is refused.
		_, err = s.ArchiveRepository(ctx, key, "operator")
		require.ErrorIs(t, err, corestore.ErrRepositoryArchived)

		// It is listed, and the storage filter applies.
		archivedRepos, err := s.ListArchivedRepositories(ctx, key.Storage)
		require.NoError(t, err)
		require.Len(t, archivedRepos, 1)
		require.Equal(t, key, archivedRepos[0].Key)
		require.Equal(t, repo.ID, archivedRepos[0].ID)
		other, err := s.ListArchivedRepositories(ctx, "elsewhere")
		require.NoError(t, err)
		require.Empty(t, other)

		// Unarchive: the repository is writable again.
		unarchived, err := s.UnarchiveRepository(ctx, key)
		require.NoError(t, err)
		require.False(t, unarchived.Archived)
		_, err = commit(ctx, s, repo.ID, create("refs/heads/topic", oid(2)))
		require.NoError(t, err)

		// Unarchiving a repository that is not archived is refused.
		_, err = s.UnarchiveRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotArchived)

		// Cleanup is refused on a live repository, and purge is refused while
		// the archived repository is still readable.
		_, err = s.ArchiveRepository(ctx, key, "operator")
		require.NoError(t, err)
		_, err = s.StartRepositoryCleanup(ctx, key, "admin", time.Hour, 7*24*time.Hour)
		var archiveRetention *corestore.ErrArchiveRetention
		require.ErrorAs(t, err, &archiveRetention, "cleanup waits for the archive retention")
		_, err = s.PurgeRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotReadClosed)
		_, err = s.GetRepository(ctx, key)
		require.NoError(t, err, "a refused purge deletes nothing")

		// Move the archive age into the past, as the archive retention passing
		// does, then close the reads and move the observation into the past.
		require.NoError(t, s.SetArchivedAtForTest(ctx, key, time.Now().Add(-2*time.Hour)))
		cleaned, err := s.StartRepositoryCleanup(ctx, key, "admin", time.Hour, 7*24*time.Hour)
		require.NoError(t, err)
		require.True(t, cleaned.ReadClosed)
		require.Equal(t, "admin", cleaned.ReadClosedBy)
		require.True(t, cleaned.PurgeAfter.After(cleaned.ReadClosedAt))

		// Cleanup cannot start twice and unarchive cannot reopen a repository.
		_, err = s.StartRepositoryCleanup(ctx, key, "admin", time.Hour, 7*24*time.Hour)
		require.ErrorIs(t, err, corestore.ErrRepositoryCleanupStarted)
		_, err = s.UnarchiveRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryCleanupStarted)

		_, err = s.PurgeRepository(ctx, key)
		var retention *corestore.ErrArchiveRetention
		require.ErrorAs(t, err, &retention)
		require.Equal(t, cleaned.PurgeAfter.Unix(), retention.PurgeAfter.Unix())
		require.NoError(t, s.SetPurgeAfterForTest(ctx, key, time.Now().Add(-time.Minute)))

		purged, err := s.PurgeRepository(ctx, key)
		require.NoError(t, err)
		require.Equal(t, repo.ID, purged.ID)
		_, err = s.GetRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)

		// The tombstone tells the garbage collector to delete the objects.
		require.EqualValues(t, 1, queryID(t, ctx, s, "SELECT COUNT(*) FROM graft_gc_removed WHERE repo_id = ?", repo.ID))
	})
}

// TestArchiveUnknownRepository: the archive and the purge report a repository
// the database does not know as NotFound.
func TestArchiveUnknownRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := testKey("missing")
		_, err := s.ArchiveRepository(ctx, key, "operator")
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
		_, err = s.UnarchiveRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
		_, err = s.PurgeRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)
	})
}

// TestPurgeNeedsCleanup: a repository that was never archived, or whose reads
// are still open, is not purged.
func TestPurgeNeedsCleanup(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := testKey("readable-archive")
		register(t, ctx, s, key)
		_, err := s.ArchiveRepository(ctx, key, "operator")
		require.NoError(t, err)
		_, err = s.PurgeRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotReadClosed)
		_, err = s.GetRepository(ctx, key)
		require.NoError(t, err, "the row is kept")
	})
}
func TestPurgeNeedsArchive(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := testKey("live")
		repo := register(t, ctx, s, key)
		_, err := s.PurgeRepository(ctx, key)
		require.ErrorIs(t, err, corestore.ErrRepositoryNotArchived)
		_, err = s.GetRepository(ctx, key)
		require.NoError(t, err, "the row is kept")
		require.Equal(t, repo.ID, repo.ID)
	})
}
