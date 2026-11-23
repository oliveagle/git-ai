package sqlstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriteTransactionReportsAFailingBegin covers both transaction paths of the
// store: SQLite begins on the dedicated connection of the write queue, Postgres
// through database/sql. A database that refuses to begin is reported either way.
func TestWriteTransactionReportsAFailingBegin(t *testing.T) {
	store, ctx := newFaultStore(t)
	store.register(t, ctx)

	faultBegin.Store(true)
	t.Cleanup(func() { faultBegin.Store(false) })
	err := store.AdvanceReconcile(ctx, 1)
	require.ErrorContains(t, err, "begin transaction")
	require.ErrorIs(t, err, errFaultQuery)

	// The Postgres path begins through database/sql instead of the write queue.
	pg := &Store{db: store.db, readDB: store.db, dialect: Postgres}
	err = pg.AdvanceReconcile(ctx, 1)
	require.ErrorContains(t, err, "begin transaction")
	require.ErrorIs(t, err, errFaultQuery)
}

func TestWriteTransactionReportsAFailingCommit(t *testing.T) {
	store, ctx := newFaultStore(t)
	store.register(t, ctx)

	faultCommit.Store(true)
	t.Cleanup(func() { faultCommit.Store(false) })
	err := store.AdvanceReconcile(ctx, 1)
	require.ErrorContains(t, err, "commit transaction")
	require.ErrorIs(t, err, errFaultQuery)
}

func TestWriteTransactionReportsAFailingBusyTimeout(t *testing.T) {
	store, ctx := newFaultStore(t)
	store.register(t, ctx)
	store.fault(t, "PRAGMA busy_timeout")

	err := store.AdvanceReconcile(ctx, 1)
	require.ErrorContains(t, err, "set busy timeout")
}

func TestWriteTransactionReportsAnUnusableConnection(t *testing.T) {
	store, ctx := newFaultStore(t)
	store.register(t, ctx)
	require.NoError(t, store.db.Close())

	err := store.AdvanceReconcile(ctx, 1)
	require.ErrorContains(t, err, "get connection")
}

func TestCloseOfAnEmptyStore(t *testing.T) {
	var store *Store
	require.NoError(t, store.Close())
	require.NoError(t, (&Store{}).Close())
}

func TestPoolValidate(t *testing.T) {
	for name, pool := range map[string]Pool{
		"no connection":       {MaxOpenConns: 0},
		"more idle than open": {MaxOpenConns: 2, MaxIdleConns: 3},
		"negative idle time":  {MaxOpenConns: 2, MaxIdleConns: 1, ConnMaxIdleTime: -1},
		"negative lifetime":   {MaxOpenConns: 2, MaxIdleConns: 1, ConnMaxLifetime: -1},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, pool.Validate())
		})
	}
	require.NoError(t, DefaultPool.Validate())
}

func TestParseDSNRejectsAnInvalidEscape(t *testing.T) {
	_, _, err := ParseDSN("sqlite:///tmp/%zz/meta.db")
	require.ErrorContains(t, err, "parse sqlite database URL")
}

func TestOpenReportsAnUnusablePool(t *testing.T) {
	_, err := OpenWithPool(context.Background(), "sqlite:///tmp/meta.db", Pool{})
	require.ErrorContains(t, err, "max open connections")
}
