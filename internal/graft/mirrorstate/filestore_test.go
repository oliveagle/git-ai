package mirrorstate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func target(name string) mirrorstate.Target {
	return mirrorstate.Target{
		Name:     name,
		URL:      "https://github.com/group/" + name + ".git",
		Branches: []string{"main", "release/*"},
		Enabled:  true,
	}
}

func TestFileStoreRegistersAndListsTargets(t *testing.T) {
	ctx := testContext(t)
	store := mirrorstate.Open(t.TempDir())

	require.NoError(t, store.RegisterTarget(ctx, mirrorstate.Key{}, target("github")))
	require.NoError(t, store.RegisterTarget(ctx, mirrorstate.Key{}, target("gitee")))

	targets, err := store.ListTargets(ctx, mirrorstate.Key{})
	require.NoError(t, err)
	require.Len(t, targets, 2)
	require.Equal(t, "gitee", targets[0].Name)
	require.Equal(t, "github", targets[1].Name)
	require.Equal(t, []string{"main", "release/*"}, targets[1].Branches)
	require.True(t, targets[1].Enabled)
}

func TestFileStoreReplacesATarget(t *testing.T) {
	ctx := testContext(t)
	store := mirrorstate.Open(t.TempDir())

	require.NoError(t, store.RegisterTarget(ctx, mirrorstate.Key{}, target("github")))

	changed := target("github")
	changed.Branches = []string{"main"}
	changed.Enabled = false
	require.NoError(t, store.RegisterTarget(ctx, mirrorstate.Key{}, changed))

	targets, err := store.ListTargets(ctx, mirrorstate.Key{})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, []string{"main"}, targets[0].Branches)
	require.False(t, targets[0].Enabled)
}

func TestFileStoreUnregister(t *testing.T) {
	ctx := testContext(t)
	store := mirrorstate.Open(t.TempDir())

	require.NoError(t, store.RegisterTarget(ctx, mirrorstate.Key{}, target("github")))
	require.NoError(t, store.UnregisterTarget(ctx, mirrorstate.Key{}, "github"))

	require.ErrorIs(t, store.UnregisterTarget(ctx, mirrorstate.Key{}, "github"), mirrorstate.ErrTargetNotFound)
	// A name that cannot be a file is not a target.
	require.ErrorIs(t, store.UnregisterTarget(ctx, mirrorstate.Key{}, "../etc"), mirrorstate.ErrTargetNotFound)

	targets, err := store.ListTargets(ctx, mirrorstate.Key{})
	require.NoError(t, err)
	require.Empty(t, targets)
}

func TestFileStoreRecordsTheSyncOfATarget(t *testing.T) {
	ctx := testContext(t)
	store := mirrorstate.Open(t.TempDir())

	published := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	require.NoError(t, store.RecordSync(ctx, mirrorstate.Key{}, mirrorstate.Sync{
		Target:      "github",
		Status:      mirrorstate.SyncOK,
		Branches:    map[string]string{"main": "abc123"},
		PublishedAt: published,
	}))

	syncs, err := store.ListSyncs(ctx, mirrorstate.Key{})
	require.NoError(t, err)
	require.Len(t, syncs, 1)
	require.Equal(t, "github", syncs[0].Target)
	require.Equal(t, mirrorstate.SyncOK, syncs[0].Status)
	require.Equal(t, "abc123", syncs[0].Published("main"))
	require.True(t, syncs[0].Synced())
	require.True(t, published.Equal(syncs[0].PublishedAt))
}

func TestFileStoreKeepsTheSyncOfARemovedTarget(t *testing.T) {
	ctx := testContext(t)
	store := mirrorstate.Open(t.TempDir())

	require.NoError(t, store.RegisterTarget(ctx, mirrorstate.Key{}, target("github")))
	require.NoError(t, store.RecordSync(ctx, mirrorstate.Key{}, mirrorstate.Sync{
		Target: "github", Status: mirrorstate.SyncOK, Branches: map[string]string{"main": "abc123"},
	}))
	require.NoError(t, store.UnregisterTarget(ctx, mirrorstate.Key{}, "github"))

	syncs, err := store.ListSyncs(ctx, mirrorstate.Key{})
	require.NoError(t, err)
	require.Len(t, syncs, 1, "the history of a removed target says what it published")
}

func TestFileStoreRefusesAnUnknownTarget(t *testing.T) {
	ctx := testContext(t)
	store := mirrorstate.Open(t.TempDir())

	require.Error(t, store.RegisterTarget(ctx, mirrorstate.Key{}, mirrorstate.Target{}))
	require.Error(t, store.RegisterTarget(ctx, mirrorstate.Key{}, mirrorstate.Target{Name: "a/b", URL: "https://example.com/a.git", Branches: []string{"main"}}))
	require.Error(t, store.RecordSync(ctx, mirrorstate.Key{}, mirrorstate.Sync{Target: "github"}))
}

func TestFileStoreEmptyStateIsEmpty(t *testing.T) {
	ctx := testContext(t)
	store := mirrorstate.Open(t.TempDir())

	targets, err := store.ListTargets(ctx, mirrorstate.Key{})
	require.NoError(t, err)
	require.Empty(t, targets)

	syncs, err := store.ListSyncs(ctx, mirrorstate.Key{})
	require.NoError(t, err)
	require.Empty(t, syncs)
}

func TestFileStoreLeavesNoTemporaryFile(t *testing.T) {
	ctx := testContext(t)
	repoPath := t.TempDir()
	store := mirrorstate.Open(repoPath)

	require.NoError(t, store.RegisterTarget(ctx, mirrorstate.Key{}, target("github")))

	entries, err := os.ReadDir(filepath.Join(repoPath, "graft", "mirror", "targets"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "github.json", entries[0].Name())
}

func TestTargetMatchesBranch(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		branch  string
		want    bool
	}{
		{pattern: "main", branch: "main", want: true},
		{pattern: "main", branch: "mainline", want: false},
		{pattern: "*", branch: "main", want: true},
		{pattern: "release/*", branch: "release/1.0", want: true},
		{pattern: "release/*", branch: "release/", want: true},
		{pattern: "release/*", branch: "main", want: false},
		{pattern: "release/*", branch: "x/release/1.0", want: false},
	} {
		target := mirrorstate.Target{Name: "t", URL: "https://example.com/a.git", Branches: []string{tc.pattern}}
		require.Equal(t, tc.want, target.MatchesBranch(tc.branch), "pattern %q against %q", tc.pattern, tc.branch)
	}
	require.False(t, mirrorstate.Target{}.MatchesBranch("main"))
}

func TestSyncSynced(t *testing.T) {
	require.True(t, mirrorstate.Sync{Target: "t", Status: mirrorstate.SyncOK}.Synced())
	require.True(t, mirrorstate.Sync{Target: "t", Status: mirrorstate.SyncDiverged}.Synced())
	require.False(t, mirrorstate.Sync{Target: "t", Status: mirrorstate.SyncFailed}.Synced())
	require.False(t, mirrorstate.Sync{Target: "t"}.Synced())
}

func TestUnregisterOfAnUnknownDirectory(t *testing.T) {
	ctx := testContext(t)
	store := mirrorstate.Open(filepath.Join(t.TempDir(), "missing"))

	err := store.UnregisterTarget(ctx, mirrorstate.Key{}, "github")
	require.True(t, errors.Is(err, mirrorstate.ErrTargetNotFound))
}
