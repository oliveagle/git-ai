package sqlstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
)

// mirrorKey returns the key of the mirror state of the repository named name:
// the state is keyed by the repository, not by its ID.
func mirrorKey(name string) mirrorstate.Key {
	key := testKey(name)
	return mirrorstate.Key{Storage: key.Storage, RelativePath: key.RelativePath}
}

// mirrorTarget returns a target of the repository.
func mirrorTarget(name string) mirrorstate.Target {
	return mirrorstate.Target{
		Name:     name,
		URL:      "https://github.com/group/" + name + ".git",
		Branches: []string{"main", "release/*"},
		Enabled:  true,
	}
}

func TestMirrorTargetsCRUD(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := mirrorKey("mirrors")

		require.NoError(t, s.RegisterTarget(ctx, key, mirrorTarget("github")))
		require.NoError(t, s.RegisterTarget(ctx, key, mirrorTarget("gitee")))

		targets, err := s.ListTargets(ctx, key)
		require.NoError(t, err)
		require.Len(t, targets, 2)
		require.Equal(t, "gitee", targets[0].Name)
		require.Equal(t, "github", targets[1].Name)
		require.Equal(t, []string{"main", "release/*"}, targets[1].Branches)
		require.True(t, targets[1].Enabled)
		require.False(t, targets[1].CreatedAt.IsZero())

		// A target registered again is replaced, not duplicated.
		changed := mirrorTarget("github")
		changed.Branches = []string{"main"}
		changed.Enabled = false
		require.NoError(t, s.RegisterTarget(ctx, key, changed))

		targets, err = s.ListTargets(ctx, key)
		require.NoError(t, err)
		require.Len(t, targets, 2)
		require.Equal(t, []string{"main"}, targets[1].Branches)
		require.False(t, targets[1].Enabled)

		require.NoError(t, s.UnregisterTarget(ctx, key, "github"))
		require.ErrorIs(t, s.UnregisterTarget(ctx, key, "github"), mirrorstate.ErrTargetNotFound)

		targets, err = s.ListTargets(ctx, key)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		require.Equal(t, "gitee", targets[0].Name)
	})
}

func TestMirrorTargetsAreKeptByRepository(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		first := mirrorKey("first")
		second := mirrorKey("second")

		require.NoError(t, s.RegisterTarget(ctx, first, mirrorTarget("github")))

		targets, err := s.ListTargets(ctx, second)
		require.NoError(t, err)
		require.Empty(t, targets)

		syncs, err := s.ListSyncs(ctx, second)
		require.NoError(t, err)
		require.Empty(t, syncs)
	})
}

func TestMirrorSyncs(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := mirrorKey("syncs")

		published := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
		require.NoError(t, s.RecordSync(ctx, key, mirrorstate.Sync{
			Target:      "github",
			Status:      mirrorstate.SyncOK,
			Branches:    map[string]string{"main": "abc123", "release/1.0": "def456"},
			PublishedAt: published,
		}))

		syncs, err := s.ListSyncs(ctx, key)
		require.NoError(t, err)
		require.Len(t, syncs, 1)
		require.Equal(t, "github", syncs[0].Target)
		require.Equal(t, mirrorstate.SyncOK, syncs[0].Status)
		require.Equal(t, "abc123", syncs[0].Published("main"))
		require.Equal(t, "def456", syncs[0].Published("release/1.0"))
		require.True(t, syncs[0].Synced())
		require.True(t, published.Equal(syncs[0].PublishedAt))

		// A later publication replaces the state of the target.
		require.NoError(t, s.RecordSync(ctx, key, mirrorstate.Sync{
			Target:   "github",
			Status:   mirrorstate.SyncFailed,
			Error:    "the remote refused the connection",
			Branches: map[string]string{"main": "abc123"},
		}))

		syncs, err = s.ListSyncs(ctx, key)
		require.NoError(t, err)
		require.Len(t, syncs, 1)
		require.Equal(t, mirrorstate.SyncFailed, syncs[0].Status)
		require.Equal(t, "the remote refused the connection", syncs[0].Error)
		require.False(t, syncs[0].Synced())
	})
}

func TestMirrorSyncOfARemovedTargetIsKept(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := mirrorKey("removed")

		require.NoError(t, s.RegisterTarget(ctx, key, mirrorTarget("github")))
		require.NoError(t, s.RecordSync(ctx, key, mirrorstate.Sync{
			Target: "github", Status: mirrorstate.SyncOK, Branches: map[string]string{"main": "abc123"},
		}))
		require.NoError(t, s.UnregisterTarget(ctx, key, "github"))

		syncs, err := s.ListSyncs(ctx, key)
		require.NoError(t, err)
		require.Len(t, syncs, 1, "the history of a removed target says what it published")
	})
}

func TestMirrorTargetIsRefusedWithoutAName(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		key := mirrorKey("invalid")
		require.Error(t, s.RegisterTarget(ctx, key, mirrorstate.Target{URL: "https://example.com/a.git", Branches: []string{"main"}}))
		require.Error(t, s.RecordSync(ctx, key, mirrorstate.Sync{Target: "github"}))
	})
}
