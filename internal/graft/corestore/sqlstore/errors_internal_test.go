package sqlstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestTheStoreClassifiesTheErrorsOfTheDatabase covers the classification of
// the errors of the database: the ones a transaction retries and the ones that
// are a unique violation.
func TestTheStoreClassifiesTheErrorsOfTheDatabase(t *testing.T) {
	require.True(t, isRetryable(sqlite3.Error{Code: sqlite3.ErrBusy}))
	require.True(t, isRetryable(sqlite3.Error{Code: sqlite3.ErrLocked}))
	require.False(t, isRetryable(sqlite3.Error{Code: sqlite3.ErrConstraint}))
	require.False(t, isRetryable(errors.New("another failure")))
	require.True(t, isRetryable(&pgconn.PgError{Code: "40001"}))
	require.True(t, isRetryable(&pgconn.PgError{Code: "40P01"}))
	require.False(t, isRetryable(&pgconn.PgError{Code: "23505"}))

	require.True(t, isBusy(sqlite3.Error{Code: sqlite3.ErrBusy}))
	require.True(t, isBusy(sqlite3.Error{Code: sqlite3.ErrLocked}))
	require.False(t, isBusy(sqlite3.Error{Code: sqlite3.ErrConstraint}))

	require.True(t, isUniqueViolation(sqlite3.Error{ExtendedCode: sqlite3.ErrConstraintUnique}))
	require.False(t, isUniqueViolation(sqlite3.Error{Code: sqlite3.ErrBusy}))
	require.True(t, isUniqueViolation(&pgconn.PgError{Code: "23505"}))
	require.False(t, isUniqueViolation(&pgconn.PgError{Code: "40001"}))
}

// TestTheStoreConvertsTheValuesOfTheDatabase covers the values the store
// writes and reads: the bytes of a reference name, the zero object ID of the
// object format, the text of a reference target and the time of the database.
func TestTheStoreConvertsTheValuesOfTheDatabase(t *testing.T) {
	require.NotNil(t, bytesArg(nil))
	require.Empty(t, bytesArg(nil))
	require.Equal(t, []byte("a"), bytesArg([]byte("a")))

	require.Len(t, zeroOID("sha1"), 40)
	require.Len(t, zeroOID("sha256"), 64)
	require.Equal(t, strings40(), zeroOID("sha1"))

	require.Nil(t, targetArg(""))
	require.Equal(t, "abc", targetArg("abc"))

	require.False(t, storableText("a\x00b"))
	require.False(t, storableText(string([]byte{0xff})))
	require.True(t, storableText("plain"))

	require.Contains(t, validUTF8(string([]byte{0xff})), "�")

	_, err := parseSQLiteTime("not a time")
	require.Error(t, err)
	when, err := parseSQLiteTime("2026-01-02T03:04:05Z")
	require.NoError(t, err)
	require.Equal(t, 2026, when.Year())
}

// ownedPackKey returns the key of a pack the repository owns.
func ownedPackKey(repoID int64, content string) string {
	return corestore.NewPackKey(repoID, sha256.Sum256([]byte(content)))
}

// strings40 returns the zero object ID of the sha1 format.
func strings40() string {
	out := make([]byte, 40)
	for i := range out {
		out[i] = '0'
	}
	return string(out)
}

// TestTheStoreRefusesTheValuesItCannotStore covers the review the store
// cannot encode and the identifier it cannot parse.
func TestTheStoreRefusesTheValuesItCannotStore(t *testing.T) {
	_, err := marshalReview(&gitalypb.Review{Id: "1", Author: string([]byte{0xff})})
	require.Error(t, err)

	body, err := marshalReview(&gitalypb.Review{Id: "1", Title: []byte("t")})
	require.NoError(t, err)
	require.Contains(t, body, "t")

	n, ok := parseReviewID("7")
	require.True(t, ok)
	require.Equal(t, int64(7), n)

	for _, id := range []string{"", "0", "01", "-1", "x", "1x"} {
		_, ok := parseReviewID(id)
		require.False(t, ok, id)
	}
}

// TestTheStoreWaitsWithTheContext covers the wait of a transaction that the
// context ends, and the error the store reports then.
func TestTheStoreWaitsWithTheContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, sleep(ctx, 0), context.Canceled)

	require.ErrorIs(t, contextError(context.Canceled, nil), context.Canceled)
	err := contextError(context.DeadlineExceeded, errors.New("the database is locked"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), "the database is locked")
}

// TestTheStoreReadsWithASnapshotOnPostgres covers the options of the read
// transaction of the two dialects.
func TestTheStoreReadsWithASnapshotOnPostgres(t *testing.T) {
	require.Nil(t, (&Store{dialect: SQLite}).readTxOptions())

	opts := (&Store{dialect: Postgres}).readTxOptions()
	require.NotNil(t, opts)
	require.True(t, opts.ReadOnly)
}

// TestTheStoreReportsTheFailuresOfTheCheckpointLifecycle covers the paths of
// the checkpoint that report a failure of the database.
func TestTheStoreReportsTheFailuresOfTheCheckpointLifecycle(t *testing.T) {
	store, ctx := newFaultStore(t)
	repo := store.register(t, ctx)
	repoID := repo.ID

	input := func() corestore.CheckpointInput {
		return corestore.CheckpointInput{
			Seq:       1,
			Refs:      map[string]corestore.Target{"refs/heads/main": "0000000000000000000000000000000000000001"},
			PackKey:   ownedPackKey(repoID, "checkpoint 1"),
			PackBytes: 42,
		}
	}

	for _, tc := range []struct {
		name  string
		query string
		call  func(ctx context.Context, s *Store) error
	}{
		{
			name:  "the repository of a checkpoint",
			query: "FROM graft_repositories",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.CreateCheckpoint(ctx, repoID, input())
				return err
			},
		},
		{
			name:  "the checkpoint at a sequence",
			query: "AND seq = ?",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.CreateCheckpoint(ctx, repoID, input())
				return err
			},
		},
		{
			name:  "the newest checkpoint",
			query: "ORDER BY seq DESC LIMIT 1",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.CreateCheckpoint(ctx, repoID, input())
				return err
			},
		},
		{
			name:  "the insert of a checkpoint",
			query: "INSERT INTO graft_checkpoints",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.CreateCheckpoint(ctx, repoID, input())
				return err
			},
		},
		{
			name:  "the insert of the references of a checkpoint",
			query: "INSERT INTO graft_checkpoint_refs",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.CreateCheckpoint(ctx, repoID, input())
				return err
			},
		},
		{
			name:  "the stamp of a checkpoint",
			query: "UPDATE graft_checkpoints SET created_at",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.CreateCheckpoint(ctx, repoID, input())
				return err
			},
		},
		{
			name:  "the list of the checkpoints of a repository",
			query: "FROM graft_checkpoints WHERE repo_id = ? ORDER BY seq",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.SyncBoundary(ctx, repoID, time.Hour)
				return err
			},
		},
		{
			name:  "the repository of a read transaction",
			query: "SELECT repo_id FROM graft_repositories WHERE repo_id = ?",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.SyncBoundary(ctx, repoID, time.Hour)
				return err
			},
		},
		{
			name:  "the scan of the checkpoints of a repository",
			query: "FROM graft_checkpoints WHERE repo_id = ? ORDER BY seq",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.GCSnapshot(ctx, repoID)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.fault(t, tc.query)
			require.Error(t, tc.call(ctx, store.Store))
		})
	}
}

// TestTheStoreRefusesTheCheckpointsItCannotWrite covers the checkpoint the
// store refuses before it writes anything.
func TestTheStoreRefusesTheCheckpointsItCannotWrite(t *testing.T) {
	store, ctx := newFaultStore(t)
	repo := store.register(t, ctx)
	repoID := repo.ID

	t.Run("a pack size that is negative", func(t *testing.T) {
		_, _, err := store.CreateCheckpoint(ctx, repoID, corestore.CheckpointInput{
			Seq:       1,
			Refs:      map[string]corestore.Target{"refs/heads/main": "0000000000000000000000000000000000000001"},
			PackKey:   ownedPackKey(repoID, "checkpoint 1"),
			PackBytes: -1,
		})
		require.Error(t, err)
	})

	t.Run("a sequence that is zero", func(t *testing.T) {
		_, _, err := store.CreateCheckpoint(ctx, repoID, corestore.CheckpointInput{
			Seq:     0,
			Refs:    map[string]corestore.Target{"refs/heads/main": "0000000000000000000000000000000000000001"},
			PackKey: ownedPackKey(repoID, "checkpoint 1"),
		})
		require.Error(t, err)
	})

	t.Run("a checkpoint without a reference", func(t *testing.T) {
		_, _, err := store.CreateCheckpoint(ctx, repoID, corestore.CheckpointInput{
			Seq:     1,
			PackKey: ownedPackKey(repoID, "checkpoint 1"),
		})
		require.Error(t, err)
	})

	t.Run("a sequence that does not fit in a signed integer", func(t *testing.T) {
		n, err := store.CheckpointsBelow(ctx, repoID, math.MaxUint64)
		require.NoError(t, err)
		require.Equal(t, 0, n)

		_, err = store.CheckpointRefs(ctx, repoID, math.MaxUint64)
		require.ErrorIs(t, err, corestore.ErrCheckpointNotFound)

		deleted, err := store.DeleteCheckpoint(ctx, repoID, math.MaxUint64)
		require.NoError(t, err)
		require.False(t, deleted)
	})
}
