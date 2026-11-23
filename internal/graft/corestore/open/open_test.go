package open

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
)

// TestOpenRefusesTheConfigurationItCannotRead covers the settings of the
// storage core that cannot be parsed.
func TestOpenRefusesTheConfigurationItCannotRead(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]config.GraftStorage{
		"mode":               {Mode: "no-such-mode"},
		"sync_timeout":       {Mode: "authoritative", SyncTimeout: "not a duration"},
		"shadow_timeout":     {Mode: "shadow", ShadowTimeout: "not a duration"},
		"gc_grace":           {Mode: "authoritative", GCGrace: "not a duration"},
		"gc_margin":          {Mode: "authoritative", GCMargin: "not a duration"},
		"object_expiration":  {Mode: "authoritative", ObjectExpiration: "not a duration"},
		"max_open_conns":     {Mode: "authoritative", DatabaseMaxOpenConns: intPtr(0)},
		"max_idle_conns":     {Mode: "authoritative", DatabaseMaxIdleConns: intPtr(-1)},
		"conn_max_idle_time": {Mode: "authoritative", DatabaseConnMaxIdleTime: "not a duration"},
	} {
		_, err := Open(context.Background(), cfg)
		require.Error(t, err, name)
	}
}

// TestOpenReturnsNothingWhenTheModeIsOff covers the storage core that is off:
// the server runs without it.
func TestOpenReturnsNothingWhenTheModeIsOff(t *testing.T) {
	t.Parallel()

	core, err := Open(context.Background(), config.GraftStorage{})
	require.NoError(t, err)
	require.Nil(t, core)

	core, err = Open(context.Background(), config.GraftStorage{Mode: "off"})
	require.NoError(t, err)
	require.Nil(t, core)
}

// TestOpenReportsTheUnreachableDatabase covers the storage core whose database
// cannot be opened.
func TestOpenReportsTheUnreachableDatabase(t *testing.T) {
	t.Parallel()

	_, err := Open(context.Background(), config.GraftStorage{
		Mode:     "authoritative",
		Database: "sqlite:///nonexistent/directory/that/cannot/be/created/meta.db",
		Bucket:   "mem://",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "open database")
}

// TestOpenReportsTheUnreachableBucket covers the storage core whose bucket
// cannot be opened after the database was.
func TestOpenReportsTheUnreachableBucket(t *testing.T) {
	t.Parallel()

	_, err := Open(context.Background(), config.GraftStorage{
		Mode:     "authoritative",
		Database: "sqlite://" + filepath.Join(t.TempDir(), "meta.db"),
		Bucket:   "no-such-scheme://bucket",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "open bucket")
}

// TestOpenOpensTheStorageCore covers the storage core that is fully
// configured, with a file database and an in-memory bucket.
func TestOpenOpensTheStorageCore(t *testing.T) {
	t.Parallel()

	core, err := Open(context.Background(), config.GraftStorage{
		Mode:     "authoritative",
		NodeID:   "node-1",
		Database: "sqlite://" + filepath.Join(t.TempDir(), "meta.db"),
		Bucket:   "mem://",
	})
	require.NoError(t, err)
	require.NotNil(t, core)
	require.Equal(t, "node-1", core.NodeID)
	require.NoError(t, core.Meta.Close())
}

func intPtr(v int) *int { return &v }
