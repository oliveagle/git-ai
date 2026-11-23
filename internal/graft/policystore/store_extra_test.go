package policystore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSnapshotFromIgnoresAnEmptySnapshot covers the context that carries no
// snapshot, and the one that carries an empty one.
func TestSnapshotFromIgnoresAnEmptySnapshot(t *testing.T) {
	_, ok := SnapshotFrom(context.Background())
	require.False(t, ok)

	ctx := WithSnapshot(context.Background(), nil)
	_, ok = SnapshotFrom(ctx)
	require.False(t, ok)

	ctx = WithSnapshot(context.Background(), &Snapshot{})
	snapshot, ok := SnapshotFrom(ctx)
	require.True(t, ok)
	require.NotNil(t, snapshot)
}

// sampleSettings is a configuration whose fields cover the three cases of
// changedSettings: a field with a TOML key, a field without one and a field
// the configuration does not export.
type sampleSettings struct {
	Exported   string `toml:"exported"`
	Untagged   string
	unexported string
}

// TestChangedSettingsNamesTheFieldsThatChanged covers the settings a reload
// must apply: the ones whose value differs, named by their TOML key.
func TestChangedSettingsNamesTheFieldsThatChanged(t *testing.T) {
	next := sampleSettings{Exported: "a", Untagged: "b", unexported: "c"}
	running := sampleSettings{Exported: "a", Untagged: "d", unexported: "e"}

	require.Equal(t, []string{"Untagged"}, changedSettings("", next, running))

	running.Exported = "z"
	changed := changedSettings("", next, running)
	require.Contains(t, changed, "exported")
	require.Contains(t, changed, "Untagged")
	require.Len(t, changed, 2)

	// The skipped settings are not named.
	running.Exported = "a"
	changed = changedSettings("graft.", next, running, "Untagged")
	require.Empty(t, changed)
}
