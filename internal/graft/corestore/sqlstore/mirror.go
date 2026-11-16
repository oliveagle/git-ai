package sqlstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
)

var _ mirrorstate.Store = (*Store)(nil)

// RegisterTarget implements mirrorstate.Store. The target of a repository is
// replaced when it is registered again: the operator changed it.
func (s *Store) RegisterTarget(ctx context.Context, key mirrorstate.Key, target mirrorstate.Target) error {
	if err := target.Validate(); err != nil {
		return err
	}

	branches, err := json.Marshal(target.Branches)
	if err != nil {
		return fmt.Errorf("encode the branches of the mirror target %q: %w", target.Name, err)
	}

	now := time.Now().UTC()
	created := target.CreatedAt
	if created.IsZero() {
		created = now
	}

	// The target is replaced, and its creation time is kept: the operator
	// changed the target, they did not add another one.
	_, err = s.db.ExecContext(ctx, s.bind(`
		INSERT INTO graft_mirror_targets (storage, relative_path, name, url, branches, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (storage, relative_path, name) DO UPDATE SET
			url = excluded.url,
			branches = excluded.branches,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at
	`), key.Storage, key.RelativePath, target.Name, target.URL, string(branches), target.Enabled,
		s.timeArg(created), s.timeArg(now))
	if err != nil {
		return fmt.Errorf("register the mirror target %q: %w", target.Name, err)
	}

	return nil
}

// UnregisterTarget implements mirrorstate.Store.
func (s *Store) UnregisterTarget(ctx context.Context, key mirrorstate.Key, name string) error {
	res, err := s.db.ExecContext(ctx, s.bind("DELETE FROM graft_mirror_targets WHERE storage = ? AND relative_path = ? AND name = ?"),
		key.Storage, key.RelativePath, name)
	if err != nil {
		return fmt.Errorf("unregister the mirror target %q: %w", name, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("unregister the mirror target %q: %w", name, err)
	}
	if n == 0 {
		return mirrorstate.ErrTargetNotFound
	}

	return nil
}

// ListTargets implements mirrorstate.Store.
func (s *Store) ListTargets(ctx context.Context, key mirrorstate.Key) ([]mirrorstate.Target, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(`
		SELECT name, url, branches, enabled, created_at, updated_at
		FROM graft_mirror_targets
		WHERE storage = ? AND relative_path = ?
		ORDER BY name
	`), key.Storage, key.RelativePath)
	if err != nil {
		return nil, fmt.Errorf("list the mirror targets: %w", err)
	}
	defer rows.Close()

	var targets []mirrorstate.Target
	for rows.Next() {
		var (
			target    mirrorstate.Target
			branches  string
			enabled   bool
			createdAt any
			updatedAt any
		)
		if err := rows.Scan(&target.Name, &target.URL, &branches, &enabled, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan a mirror target: %w", err)
		}
		if err := json.Unmarshal([]byte(branches), &target.Branches); err != nil {
			return nil, fmt.Errorf("decode the branches of the mirror target %q: %w", target.Name, err)
		}
		target.Enabled = enabled
		if target.CreatedAt, err = scanTime(createdAt); err != nil {
			return nil, fmt.Errorf("read the creation time of the mirror target %q: %w", target.Name, err)
		}
		if target.UpdatedAt, err = scanTime(updatedAt); err != nil {
			return nil, fmt.Errorf("read the update time of the mirror target %q: %w", target.Name, err)
		}
		targets = append(targets, target)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the mirror targets: %w", err)
	}

	return targets, nil
}

// RecordSync implements mirrorstate.Store.
func (s *Store) RecordSync(ctx context.Context, key mirrorstate.Key, sync mirrorstate.Sync) error {
	if err := sync.Validate(); err != nil {
		return err
	}

	branches, err := json.Marshal(sync.Branches)
	if err != nil {
		return fmt.Errorf("encode the branches of the mirror synchronisation %q: %w", sync.Target, err)
	}

	published := sync.PublishedAt
	if published.IsZero() {
		published = time.Now().UTC()
	}

	_, err = s.db.ExecContext(ctx, s.bind(`
		INSERT INTO graft_mirror_syncs (storage, relative_path, name, status, branches, published_at, error)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (storage, relative_path, name) DO UPDATE SET
			status = excluded.status,
			branches = excluded.branches,
			published_at = excluded.published_at,
			error = excluded.error
	`), key.Storage, key.RelativePath, sync.Target, sync.Status, string(branches), s.timeArg(published), sync.Error)
	if err != nil {
		return fmt.Errorf("record the synchronisation of the mirror target %q: %w", sync.Target, err)
	}

	return nil
}

// ListSyncs implements mirrorstate.Store.
func (s *Store) ListSyncs(ctx context.Context, key mirrorstate.Key) ([]mirrorstate.Sync, error) {
	rows, err := s.db.QueryContext(ctx, s.bind(`
		SELECT name, status, branches, published_at, error
		FROM graft_mirror_syncs
		WHERE storage = ? AND relative_path = ?
		ORDER BY name
	`), key.Storage, key.RelativePath)
	if err != nil {
		return nil, fmt.Errorf("list the mirror synchronisations: %w", err)
	}
	defer rows.Close()

	var syncs []mirrorstate.Sync
	for rows.Next() {
		var (
			sync        mirrorstate.Sync
			branches    string
			publishedAt any
		)
		if err := rows.Scan(&sync.Target, &sync.Status, &branches, &publishedAt, &sync.Error); err != nil {
			return nil, fmt.Errorf("scan a mirror synchronisation: %w", err)
		}
		if err := json.Unmarshal([]byte(branches), &sync.Branches); err != nil {
			return nil, fmt.Errorf("decode the branches of the mirror synchronisation %q: %w", sync.Target, err)
		}
		if sync.PublishedAt, err = scanTime(publishedAt); err != nil {
			return nil, fmt.Errorf("read the publication time of the mirror synchronisation %q: %w", sync.Target, err)
		}
		syncs = append(syncs, sync)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the mirror synchronisations: %w", err)
	}

	return syncs, nil
}
