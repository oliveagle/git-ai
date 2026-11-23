package sqlstore

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
)

// TestTheStoreReportsTheMirrorRowsItCannotScan covers the two values of a
// mirror row that a database can hold and the store cannot read: a flag that
// is not a flag, and a time that is not a time. A broken row is reported with
// the table and the field it came from, so that an operator knows what to
// repair.
func TestTheStoreReportsTheMirrorRowsItCannotScan(t *testing.T) {
	key := mirrorstate.Key{Storage: "default", RelativePath: "@hashed/x.git"}

	t.Run("a flag that is not a flag", func(t *testing.T) {
		store, ctx := newFaultStore(t)
		store.register(t, ctx)
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO graft_mirror_targets (storage, relative_path, name, url, branches, enabled, created_at, updated_at)
			VALUES (?, ?, 'gitee', 'https://gitee.com/x/y.git', '[]', 'maybe', '2026-01-02T03:04:05Z', '2026-01-02T03:04:05Z')`,
			key.Storage, key.RelativePath)
		require.NoError(t, err)

		_, err = store.ListTargets(ctx, key)
		require.ErrorContains(t, err, "scan a mirror target")
	})

	t.Run("the update time of a mirror target", func(t *testing.T) {
		store, ctx := newFaultStore(t)
		store.register(t, ctx)
		_, err := store.db.ExecContext(ctx, `
			INSERT INTO graft_mirror_targets (storage, relative_path, name, url, branches, enabled, created_at, updated_at)
			VALUES (?, ?, 'gitee', 'https://gitee.com/x/y.git', '[]', 1, '2026-01-02T03:04:05Z', X'00')`,
			key.Storage, key.RelativePath)
		require.NoError(t, err)

		_, err = store.ListTargets(ctx, key)
		require.ErrorContains(t, err, "read the update time of the mirror target")
	})
}
