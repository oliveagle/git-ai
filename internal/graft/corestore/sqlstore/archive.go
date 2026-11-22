package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
)

// ArchiveRepository implements corestore.MetaStore.
//
// The repository keeps its row: it stays tracked, its name stays taken and its
// objects stay in the bucket. It remains readable, but no update may change it
// any more. The archive state of the row holds the time of the archive and the
// identity that archived it, both on the database clock (doc/graft/PLAN13.md,
// ARCHIVE RULE).
func (s *Store) ArchiveRepository(ctx context.Context, key corestore.RepoKey, archivedBy string) (corestore.Repository, error) {
	if !storableKey(key) {
		return corestore.Repository{}, corestore.ErrRepositoryNotFound
	}

	var repo corestore.Repository
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		state, err := s.selectRepository(ctx, tx, "storage = ? AND relative_path = ?", []any{key.Storage, key.RelativePath}, true)
		if err != nil {
			return err
		}
		if state.archived {
			return corestore.ErrRepositoryArchived
		}

		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, s.bind(
			"UPDATE graft_repositories SET archived_at = ?, archived_by = ?, read_closed_at = NULL, read_closed_by = NULL, purge_after = NULL WHERE repo_id = ?"),
			s.timeArg(now), archivedBy, state.id)
		if err != nil {
			return fmt.Errorf("archive the repository: %w", err)
		}

		repo = toRepository(state)
		repo.Archived = true
		repo.ArchivedAt = now
		repo.ArchivedBy = archivedBy
		repo.ReadClosed = false
		repo.ReadClosedAt = time.Time{}
		repo.ReadClosedBy = ""
		repo.PurgeAfter = time.Time{}
		return nil
	})
	if err != nil {
		return corestore.Repository{}, err
	}
	return repo, nil
}

// StartRepositoryCleanup implements corestore.MetaStore: it is the admin-only
// first step of deletion. The archive age, the close of the reads and the start
// of the observation period are one transaction on the database clock.
func (s *Store) StartRepositoryCleanup(ctx context.Context, key corestore.RepoKey, closedBy string, archiveRetention, observation time.Duration) (corestore.Repository, error) {
	if archiveRetention <= 0 || observation <= 0 {
		return corestore.Repository{}, fmt.Errorf("storage core: invalid cleanup times: archive retention %s, observation %s", archiveRetention, observation)
	}
	if !storableKey(key) {
		return corestore.Repository{}, corestore.ErrRepositoryNotFound
	}

	var repo corestore.Repository
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		state, err := s.selectRepository(ctx, tx, "storage = ? AND relative_path = ?", []any{key.Storage, key.RelativePath}, true)
		if err != nil {
			return err
		}
		if !state.archived {
			return corestore.ErrRepositoryNotArchived
		}
		if state.readClosed {
			return corestore.ErrRepositoryCleanupStarted
		}

		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}
		cleanupAfter := state.archivedAt.Add(archiveRetention)
		purgeable := now.Add(observation)
		if now.Before(cleanupAfter) {
			return &corestore.ErrArchiveRetention{PurgeAfter: cleanupAfter}
		}

		_, err = tx.ExecContext(ctx, s.bind(
			"UPDATE graft_repositories SET read_closed_at = ?, read_closed_by = ?, purge_after = ? WHERE repo_id = ?"),
			s.timeArg(now), closedBy, s.timeArg(purgeable), state.id)
		if err != nil {
			return fmt.Errorf("start the cleanup of the repository: %w", err)
		}

		repo = toRepository(state)
		repo.ReadClosed = true
		repo.ReadClosedAt = now
		repo.ReadClosedBy = closedBy
		repo.PurgeAfter = purgeable
		return nil
	})
	if err != nil {
		return corestore.Repository{}, err
	}
	return repo, nil
}

// UnarchiveRepository implements corestore.MetaStore.
func (s *Store) UnarchiveRepository(ctx context.Context, key corestore.RepoKey) (corestore.Repository, error) {
	if !storableKey(key) {
		return corestore.Repository{}, corestore.ErrRepositoryNotFound
	}

	var repo corestore.Repository
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		state, err := s.selectRepository(ctx, tx, "storage = ? AND relative_path = ?", []any{key.Storage, key.RelativePath}, true)
		if err != nil {
			return err
		}
		if !state.archived {
			return corestore.ErrRepositoryNotArchived
		}
		if state.readClosed {
			return corestore.ErrRepositoryCleanupStarted
		}

		_, err = tx.ExecContext(ctx, s.bind(
			"UPDATE graft_repositories SET archived_at = NULL, archived_by = NULL, read_closed_at = NULL, read_closed_by = NULL, purge_after = NULL WHERE repo_id = ?"),
			state.id)
		if err != nil {
			return fmt.Errorf("unarchive the repository: %w", err)
		}

		repo = toRepository(state)
		repo.Archived = false
		repo.ArchivedAt = time.Time{}
		repo.ArchivedBy = ""
		repo.ReadClosed = false
		repo.ReadClosedAt = time.Time{}
		repo.ReadClosedBy = ""
		repo.PurgeAfter = time.Time{}
		return nil
	})
	if err != nil {
		return corestore.Repository{}, err
	}
	return repo, nil
}

// ListArchivedRepositories implements corestore.MetaStore.
func (s *Store) ListArchivedRepositories(ctx context.Context, storage string) ([]corestore.Repository, error) {
	query := "SELECT " + repoColumns + " FROM graft_repositories WHERE archived_at IS NOT NULL"
	var args []any
	if storage != "" {
		query += " AND storage = ?"
		args = append(args, storage)
	}
	query += " ORDER BY repo_id"

	var repos []corestore.Repository
	err := s.withReadTx(ctx, func(tx *sql.Tx) error {
		repos = nil

		rows, err := tx.QueryContext(ctx, s.bind(query), args...)
		if err != nil {
			return fmt.Errorf("list the archived repositories: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			r, err := scanRepoState(rows.Scan)
			if err != nil {
				return fmt.Errorf("scan the archived repository: %w", err)
			}
			repos = append(repos, toRepository(r))
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return repos, nil
}

// PurgeRepository implements corestore.MetaStore.
func (s *Store) PurgeRepository(ctx context.Context, key corestore.RepoKey) (corestore.Repository, error) {
	if !storableKey(key) {
		return corestore.Repository{}, corestore.ErrRepositoryNotFound
	}

	var repo corestore.Repository
	err := s.withTx(ctx, nil, func(tx *sql.Tx) error {
		state, err := s.selectRepository(ctx, tx, "storage = ? AND relative_path = ?", []any{key.Storage, key.RelativePath}, true)
		if err != nil {
			return err
		}
		if !state.archived {
			return corestore.ErrRepositoryNotArchived
		}
		if !state.readClosed {
			return corestore.ErrRepositoryNotReadClosed
		}

		// The observation period is checked on the database clock, in the
		// transaction that deletes the row: the decision and the deletion are
		// one.
		now, err := s.wallTime(ctx, tx)
		if err != nil {
			return err
		}
		if now.Before(state.purgeAfter) {
			return &corestore.ErrArchiveRetention{PurgeAfter: state.purgeAfter}
		}

		// The cascade removes everything recorded for the repository. The
		// trigger graft_repositories_tombstone records its tombstone, on the
		// database clock, in this transaction. Garbage collection waits for
		// object_expiration (never shorter than gc_grace) before it deletes the
		// bucket prefix.
		if _, err := tx.ExecContext(ctx, s.bind("DELETE FROM graft_repositories WHERE repo_id = ?"), state.id); err != nil {
			return fmt.Errorf("delete repository: %w", err)
		}

		repo = toRepository(state)
		return nil
	})
	if err != nil {
		return corestore.Repository{}, err
	}
	return repo, nil
}
