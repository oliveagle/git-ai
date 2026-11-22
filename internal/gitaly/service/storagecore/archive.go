package storagecore

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/gitaly/repoutil"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// ArchiveRepository implements gitalypb.StorageCoreServiceServer.
//
// It archives the repository: it stays tracked and readable, but no update may
// change it any more. Cleanup is a separate, admin-only step.
func (s *server) ArchiveRepository(ctx context.Context, in *gitalypb.ArchiveRepositoryRequest) (*gitalypb.ArchiveRepositoryResponse, error) {
	core, err := s.authoritativeCore()
	if err != nil {
		return nil, err
	}

	repo := in.GetRepository()
	if err := s.validateRepository(ctx, repo); err != nil {
		return nil, err
	}

	record, err := core.Meta.ArchiveRepository(ctx, corestore.KeyOf(repo), identity.Actor(ctx, "unknown"))
	switch {
	case errors.Is(err, corestore.ErrRepositoryNotFound):
		return nil, structerr.NewNotFound("the repository is not tracked by the storage core")
	case errors.Is(err, corestore.ErrRepositoryArchived):
		return nil, structerr.NewFailedPrecondition("the repository is archived already")
	case err != nil:
		return nil, structerr.NewUnavailable("storage core: archive the repository: %w", err)
	}

	s.deps.GetLogger().WithFields(map[string]any{
		"repository": record.Key.String(),
		"repo_id":    record.ID,
		"actor":      record.ArchivedBy,
	}).Info("storage core: a repository is archived")

	return &gitalypb.ArchiveRepositoryResponse{
		ArchivedAt: timestampOrNil(record.ArchivedAt),
		ArchivedBy: record.ArchivedBy,
	}, nil
}

// UnarchiveRepository implements gitalypb.StorageCoreServiceServer. It is
// refused after cleanup has closed the reads of the repository.
func (s *server) UnarchiveRepository(ctx context.Context, in *gitalypb.UnarchiveRepositoryRequest) (*gitalypb.UnarchiveRepositoryResponse, error) {
	core, err := s.authoritativeCore()
	if err != nil {
		return nil, err
	}

	repo := in.GetRepository()
	if err := s.validateRepository(ctx, repo); err != nil {
		return nil, err
	}

	record, err := core.Meta.UnarchiveRepository(ctx, corestore.KeyOf(repo))
	switch {
	case errors.Is(err, corestore.ErrRepositoryNotFound):
		return nil, structerr.NewNotFound("the repository is not tracked by the storage core")
	case errors.Is(err, corestore.ErrRepositoryNotArchived):
		return nil, structerr.NewFailedPrecondition("the repository is not archived")
	case errors.Is(err, corestore.ErrRepositoryCleanupStarted):
		return nil, structerr.NewFailedPrecondition("the cleanup of the repository has started: its reads are closed")
	case err != nil:
		return nil, structerr.NewUnavailable("storage core: unarchive the repository: %w", err)
	}

	s.deps.GetLogger().WithFields(map[string]any{
		"repository": record.Key.String(),
		"repo_id":    record.ID,
	}).Info("storage core: a repository is taken out of the archive")

	return &gitalypb.UnarchiveRepositoryResponse{}, nil
}

// StartRepositoryCleanup implements gitalypb.StorageCoreServiceServer. It is
// the first, irreversible cleanup step: only a super admin may close the reads
// of an archived repository, and the observation period starts at once.
func (s *server) StartRepositoryCleanup(ctx context.Context, in *gitalypb.StartRepositoryCleanupRequest) (*gitalypb.StartRepositoryCleanupResponse, error) {
	core, err := s.authoritativeCore()
	if err != nil {
		return nil, err
	}

	if id, ok := identity.FromContext(ctx); !ok || id.Source != identity.SourceAdmin {
		return nil, structerr.NewPermissionDenied("starting a cleanup needs the super admin's token (graft.admin_token)")
	}

	retention, err := s.archiveRetention()
	if err != nil {
		return nil, err
	}
	observation, err := s.cleanupObservation()
	if err != nil {
		return nil, err
	}

	repo := in.GetRepository()
	if err := s.validateRepository(ctx, repo); err != nil {
		return nil, err
	}

	record, err := core.Meta.StartRepositoryCleanup(ctx, corestore.KeyOf(repo), identity.Actor(ctx, "unknown"), retention, observation)
	var retentionErr *corestore.ErrArchiveRetention
	switch {
	case errors.Is(err, corestore.ErrRepositoryNotFound):
		return nil, structerr.NewNotFound("the repository is not tracked by the storage core")
	case errors.Is(err, corestore.ErrRepositoryNotArchived):
		return nil, structerr.NewFailedPrecondition("the repository is not archived: cleanup starts only on an archived repository")
	case errors.Is(err, corestore.ErrRepositoryCleanupStarted):
		return nil, structerr.NewFailedPrecondition("the cleanup of the repository has started already")
	case errors.As(err, &retentionErr):
		return nil, structerr.NewFailedPrecondition("cleanup may start from %s, after the archive retention",
			retentionErr.PurgeAfter.UTC().Format(time.RFC3339))
	case err != nil:
		return nil, structerr.NewUnavailable("storage core: start the cleanup of the repository: %w", err)
	}

	s.deps.GetLogger().WithFields(map[string]any{
		"repository":  record.Key.String(),
		"repo_id":     record.ID,
		"actor":       record.ReadClosedBy,
		"purge_after": record.PurgeAfter,
		"observation": observation.String(),
	}).Info("storage core: cleanup closed the reads of a repository")

	return &gitalypb.StartRepositoryCleanupResponse{
		ReadClosedAt: timestampOrNil(record.ReadClosedAt),
		ReadClosedBy: record.ReadClosedBy,
		PurgeAfter:   timestampOrNil(record.PurgeAfter),
	}, nil
}

// ListArchivedRepositories implements gitalypb.StorageCoreServiceServer.
func (s *server) ListArchivedRepositories(ctx context.Context, in *gitalypb.ListArchivedRepositoriesRequest) (*gitalypb.ListArchivedRepositoriesResponse, error) {
	core, err := s.authoritativeCore()
	if err != nil {
		return nil, err
	}

	records, err := core.Meta.ListArchivedRepositories(ctx, in.GetStorageName())
	if err != nil {
		return nil, structerr.NewUnavailable("storage core: list the archived repositories: %w", err)
	}

	resp := &gitalypb.ListArchivedRepositoriesResponse{
		Repositories: make([]*gitalypb.ArchivedRepository, 0, len(records)),
	}
	for _, record := range records {
		resp.Repositories = append(resp.Repositories, &gitalypb.ArchivedRepository{
			StorageName:  record.Key.Storage,
			RelativePath: record.Key.RelativePath,
			RepositoryId: record.ID,
			ArchivedAt:   timestampOrNil(record.ArchivedAt),
			ArchivedBy:   record.ArchivedBy,
			ReadClosedAt: timestampOrNil(record.ReadClosedAt),
			ReadClosedBy: record.ReadClosedBy,
			PurgeAfter:   timestampOrNil(record.PurgeAfter),
		})
	}
	return resp, nil
}

// PurgeRepository implements gitalypb.StorageCoreServiceServer.
//
// It deletes an archived repository whose cleanup observation has passed. Only
// the super admin may call it: the archive and the read-closed observation are
// the two safety stops before the deletion.
func (s *server) PurgeRepository(ctx context.Context, in *gitalypb.PurgeRepositoryRequest) (*gitalypb.PurgeRepositoryResponse, error) {
	core, err := s.authoritativeCore()
	if err != nil {
		return nil, err
	}

	if id, ok := identity.FromContext(ctx); !ok || id.Source != identity.SourceAdmin {
		return nil, structerr.NewPermissionDenied("purging a repository needs the super admin's token (graft.admin_token)")
	}

	repo := in.GetRepository()
	if err := s.validateRepository(ctx, repo); err != nil {
		return nil, err
	}

	// The row goes first, so that a request that arrives meanwhile does not
	// find a repository that is being deleted. Its tombstone makes the
	// garbage collector delete the objects object_expiration later.
	record, err := core.Meta.PurgeRepository(ctx, corestore.KeyOf(repo))
	var retention *corestore.ErrArchiveRetention
	switch {
	case errors.Is(err, corestore.ErrRepositoryNotFound):
		return nil, structerr.NewNotFound("the repository is not tracked by the storage core")
	case errors.Is(err, corestore.ErrRepositoryNotArchived):
		return nil, structerr.NewFailedPrecondition("the repository is not archived: only an archived repository may be purged")
	case errors.Is(err, corestore.ErrRepositoryNotReadClosed):
		return nil, structerr.NewFailedPrecondition("the repository is still readable: start the cleanup with graft storage cleanup-start before it can be purged")
	case errors.As(err, &retention):
		return nil, structerr.NewFailedPrecondition("the repository is read-closed until %s: it may be purged then",
			retention.PurgeAfter.UTC().Format(time.RFC3339))
	case err != nil:
		return nil, structerr.NewUnavailable("storage core: purge the repository: %w", err)
	}

	// The local copy of this node goes the way the removal of a repository
	// goes: into the temporary directory of the storage, from where Gitaly
	// deletes it. The local copies of the other nodes are stale caches of a
	// repository the database does not know any more: they are moved aside
	// by the next request that reaches them.
	if err := repoutil.Remove(ctx, s.deps.GetLogger(), s.deps.GetLocator(), s.deps.GetTxManager(), s.deps.GetRepositoryCounter(), repo); err != nil {
		s.deps.GetLogger().WithError(err).Warn("storage core: removing the local copy of a purged repository")
	}

	s.deps.GetLogger().WithFields(map[string]any{
		"repository": record.Key.String(),
		"repo_id":    record.ID,
	}).Info("storage core: an archived repository is purged")

	return &gitalypb.PurgeRepositoryResponse{RepositoryId: record.ID}, nil
}

// timestampOrNil preserves the unset state of a lifecycle time. A Go zero time
// must not become 1970-01-01 in the response.
func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// authoritativeCore returns the storage core, or FailedPrecondition when it is
// off or not authoritative. The archive owns the state of the repository, and
// only the authoritative mode has the database as its source of truth.
func (s *server) authoritativeCore() (*corestore.Core, error) {
	core := s.deps.GetGraftStorage()
	if !core.Enabled() {
		return nil, structerr.NewFailedPrecondition("the storage core is off")
	}
	if !core.Authoritative() {
		return nil, structerr.NewFailedPrecondition("the repository archive needs the authoritative storage mode")
	}
	return core, nil
}

// archiveRetention returns how long an archived repository remains readable
// before a super admin may start its cleanup.
func (s *server) archiveRetention() (time.Duration, error) {
	retention, err := s.deps.Cfg.Graft.Storage.ArchiveRetentionOrDefault()
	if err != nil {
		return 0, structerr.NewInvalidArgument("%w", err)
	}
	return retention, nil
}

// cleanupObservation returns the minimum read-closed observation period before
// a purge.
func (s *server) cleanupObservation() (time.Duration, error) {
	observation, err := s.deps.Cfg.Graft.Storage.CleanupObservationOrDefault()
	if err != nil {
		return 0, structerr.NewInvalidArgument("%w", err)
	}
	return observation, nil
}
