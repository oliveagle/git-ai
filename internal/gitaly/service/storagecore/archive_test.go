package storagecore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// adminTestToken is the super admin's token of the lifecycle tests.
const adminTestToken = "admin-test-token"

// storageClient dials the storage core service with token.
func storageClient(t *testing.T, ctx context.Context, cfg config.Cfg, token string) gitalypb.StorageCoreServiceClient {
	t.Helper()

	conn, err := client.New(ctx, cfg.SocketPath, client.WithGrpcOptions([]grpc.DialOption{
		client.UnaryInterceptor(),
		client.StreamInterceptor(),
		grpc.WithPerRPCCredentials(gitalyauth.RPCCredentialsV2(token)),
	}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return gitalypb.NewStorageCoreServiceClient(conn)
}

// setupArchive starts an authoritative server whose super admin is
// adminTestToken and whose archive retention is negligible. The cleanup
// observation keeps its production floor, so tests move its deadline explicitly.
func setupArchive(t *testing.T, dialect sqlstore.Dialect) (*harness, gitalypb.StorageCoreServiceClient) {
	t.Helper()

	var admin gitalypb.StorageCoreServiceClient
	h := setupWithConfig(t, corestore.ModeAuthoritative, dialect, func(cfg *config.Cfg) {
		cfg.Graft.AdminToken = adminTestToken
		cfg.Graft.Storage.ArchiveRetention = "1ns"
	})
	admin = storageClient(t, h.ctx, h.cfg, adminTestToken)
	return h, admin
}

// TestRepositoryArchiveLifecycle: archive freezes writes and keeps reads; the
// admin-only cleanup closes reads and starts an observation period; only then
// may the super admin purge the repository.
func TestRepositoryArchiveLifecycle(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h, admin := setupArchive(t, dialect)
		h.seed()
		repoID := h.importRepo()

		archived, err := h.client.ArchiveRepository(h.ctx, &gitalypb.ArchiveRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, "shared", archived.GetArchivedBy(), "the caller's authenticated identity is recorded")
		require.Nil(t, archived.GetPurgeAfter(), "cleanup has not started yet")

		state, err := h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.True(t, state.GetTracked())
		require.Equal(t, repoID, state.GetRepositoryId())
		require.True(t, state.GetArchived())
		require.False(t, state.GetReadClosed())
		require.Nil(t, state.GetPurgeAfter())

		listed, err := h.client.ListArchivedRepositories(h.ctx, &gitalypb.ListArchivedRepositoriesRequest{StorageName: h.repo.GetStorageName()})
		require.NoError(t, err)
		require.Len(t, listed.GetRepositories(), 1)
		require.Equal(t, h.repo.GetRelativePath(), listed.GetRepositories()[0].GetRelativePath())
		require.False(t, listed.GetRepositories()[0].GetReadClosedAt().AsTime().After(time.Unix(1, 0)))

		// The store refuses every write to the frozen repository.
		_, err = h.core.Meta.CommitChangeset(h.ctx, corestore.ChangesetInput{
			RepoID:    repoID,
			Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectAny, New: corestore.Target("0123456789abcdef0123456789abcdef01234567")}},
			Operation: "test",
			Actor:     "operator",
			NodeID:    h.cfg.Graft.Storage.NodeID,
		})
		require.ErrorIs(t, err, corestore.ErrRepositoryArchived)

		// Archiving twice is refused.
		_, err = h.client.ArchiveRepository(h.ctx, &gitalypb.ArchiveRepositoryRequest{Repository: h.repo})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)

		// Before cleanup starts, unarchive remains the way back.
		_, err = h.client.UnarchiveRepository(h.ctx, &gitalypb.UnarchiveRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		state, err = h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.False(t, state.GetArchived())
		require.Nil(t, state.GetPurgeAfter())
		_, err = h.core.Meta.CommitChangeset(h.ctx, corestore.ChangesetInput{
			RepoID:    repoID,
			Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectAny, New: corestore.Target("0123456789abcdef0123456789abcdef01234567")}},
			Operation: "test",
			Actor:     "operator",
			NodeID:    h.cfg.Graft.Storage.NodeID,
		})

		// Archive again. Cleanup is admin-only, and it is refused before the
		// negligible archive retention has passed.
		_, err = h.client.ArchiveRepository(h.ctx, &gitalypb.ArchiveRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		_, err = h.client.StartRepositoryCleanup(h.ctx, &gitalypb.StartRepositoryCleanupRequest{Repository: h.repo})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "the shared token may not start cleanup: %v", err)
		time.Sleep(2 * time.Millisecond)

		cleaned, err := admin.StartRepositoryCleanup(h.ctx, &gitalypb.StartRepositoryCleanupRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, "admin", cleaned.GetReadClosedBy())
		require.True(t, cleaned.GetPurgeAfter().AsTime().Sub(cleaned.GetReadClosedAt().AsTime()) >= 7*24*time.Hour)
		require.Equal(t, "admin", cleaned.GetReadClosedBy())
		require.True(t, cleaned.GetReadClosedAt().AsTime().After(time.Unix(1, 0)))
		require.True(t, cleaned.GetPurgeAfter().AsTime().Sub(cleaned.GetReadClosedAt().AsTime()) >= 7*24*time.Hour)

		// Cleanup is irreversible: unarchive is refused. A second start is
		// refused too.
		_, err = h.client.UnarchiveRepository(h.ctx, &gitalypb.UnarchiveRepositoryRequest{Repository: h.repo})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
		_, err = admin.StartRepositoryCleanup(h.ctx, &gitalypb.StartRepositoryCleanupRequest{Repository: h.repo})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)

		state, err = h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.True(t, state.GetReadClosed())
		require.Equal(t, cleaned.GetReadClosedAt().AsTime().Unix(), state.GetReadClosedAt().AsTime().Unix())
		require.Equal(t, cleaned.GetPurgeAfter().AsTime().Unix(), state.GetPurgeAfter().AsTime().Unix())

		// The observation period is enforced on the database clock. Move it to
		// the past, as waiting seven days does, then purge as the super admin.
		_, err = admin.PurgeRepository(h.ctx, &gitalypb.PurgeRepositoryRequest{Repository: h.repo})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "the observation period has not passed: %v", err)
		require.NoError(t, setPurgeAfter(h, repoID, time.Now().Add(-time.Minute)))

		purged, err := admin.PurgeRepository(h.ctx, &gitalypb.PurgeRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, repoID, purged.GetRepositoryId())

		state, err = h.client.GetStorageState(h.ctx, &gitalypb.GetStorageStateRequest{Repository: h.repo})
		require.NoError(t, err)
		require.False(t, state.GetTracked())
		listed, err = h.client.ListArchivedRepositories(h.ctx, &gitalypb.ListArchivedRepositoriesRequest{StorageName: h.repo.GetStorageName()})
		require.NoError(t, err)
		require.Empty(t, listed.GetRepositories())
		require.NoDirExists(t, h.repoPath)
	})
}

// TestPurgeRefusals: purge needs the authoritative core, an archived repository,
// and a cleanup that has closed its reads.
func TestPurgeRefusals(t *testing.T) {
	t.Parallel()

	shadow, admin := setupArchiveMode(t, corestore.ModeShadow)
	_ = shadow
	_, err := admin.PurgeRepository(shadow.ctx, &gitalypb.PurgeRepositoryRequest{Repository: shadow.repo})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)

	live, admin := setupArchive(t, sqlstore.SQLite)
	live.seed()
	live.importRepo()
	_, err = admin.PurgeRepository(live.ctx, &gitalypb.PurgeRepositoryRequest{Repository: live.repo})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "a repository that is not archived is not purged: %v", err)
	_, err = live.client.ArchiveRepository(live.ctx, &gitalypb.ArchiveRepositoryRequest{Repository: &gitalypb.Repository{StorageName: live.repo.GetStorageName(), RelativePath: "missing.git"}})
	require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	_, err = live.client.ArchiveRepository(live.ctx, &gitalypb.ArchiveRepositoryRequest{Repository: live.repo})
	require.NoError(t, err)
	_, err = admin.PurgeRepository(live.ctx, &gitalypb.PurgeRepositoryRequest{Repository: live.repo})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "an archived repository is still readable: %v", err)
}

// setupArchiveMode is setupArchive for a mode other than authoritative: the
// admin client is dialed against the same server.
func setupArchiveMode(t *testing.T, mode corestore.Mode) (*harness, gitalypb.StorageCoreServiceClient) {
	t.Helper()

	var admin gitalypb.StorageCoreServiceClient
	h := setupWithConfig(t, mode, sqlstore.SQLite, func(cfg *config.Cfg) {
		cfg.Graft.AdminToken = adminTestToken
	})
	admin = storageClient(t, h.ctx, h.cfg, adminTestToken)
	return h, admin
}

// TestCleanupObservation: purge is refused until the read-closed observation
// has passed; the deadline is checked in the delete transaction.
func TestCleanupObservation(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, dialect sqlstore.Dialect) {
		h, admin := setupArchive(t, dialect)
		h.seed()
		repoID := h.importRepo()

		_, err := h.client.ArchiveRepository(h.ctx, &gitalypb.ArchiveRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		time.Sleep(2 * time.Millisecond)
		cleaned, err := admin.StartRepositoryCleanup(h.ctx, &gitalypb.StartRepositoryCleanupRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, "admin", cleaned.GetReadClosedBy())
		require.True(t, cleaned.GetPurgeAfter().AsTime().Sub(cleaned.GetReadClosedAt().AsTime()) >= 7*24*time.Hour)

		_, err = admin.PurgeRepository(h.ctx, &gitalypb.PurgeRepositoryRequest{Repository: h.repo})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), "the observation period has not passed: %v", err)
		record, err := h.core.Meta.GetRepository(h.ctx, corestore.KeyOf(h.repo))
		require.NoError(t, err, "a refused purge deletes nothing")
		require.True(t, record.ReadClosed)

		require.NoError(t, setPurgeAfter(h, repoID, time.Now().Add(-time.Minute)))
		purged, err := admin.PurgeRepository(h.ctx, &gitalypb.PurgeRepositoryRequest{Repository: h.repo})
		require.NoError(t, err)
		require.Equal(t, repoID, purged.GetRepositoryId())
	})
}

// TestArchiveNeedsAuthoritativeCore: the archive is the authoritative state of
// the repository, so shadow mode refuses it.
func TestArchiveNeedsAuthoritativeCore(t *testing.T) {
	t.Parallel()

	shadow := setup(t, corestore.ModeShadow, sqlstore.SQLite)
	shadow.seed()
	shadow.importRepo()
	_, err := shadow.client.ArchiveRepository(shadow.ctx, &gitalypb.ArchiveRepositoryRequest{Repository: shadow.repo})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "%v", err)
}

// setPurgeAfter moves the cleanup deadline into the past, as waiting for the
// observation period does. The value uses the store's dialect.
func setPurgeAfter(h *harness, repoID int64, when time.Time) error {
	when = when.UTC()
	var value any = when.Format(time.RFC3339Nano)
	if h.core.Meta.(*sqlstore.Store).Dialect() == sqlstore.Postgres {
		value = when
	}
	h.exec("UPDATE graft_repositories SET purge_after = ? WHERE repo_id = ?", value, repoID)
	return nil
}
