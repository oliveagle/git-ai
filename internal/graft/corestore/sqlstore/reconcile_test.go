package sqlstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
)

// insertAllocatorMark records an allocator mark the API cannot create: one with
// the age the test needs.
func insertAllocatorMark(t *testing.T, ctx context.Context, s *sqlstore.Store, observedAt time.Time, position int64) {
	t.Helper()

	execSQL(t, ctx, s, "INSERT INTO graft_gc_allocator_marks (observed_at, position) VALUES (?, ?)", timeValue(s, observedAt), position)
}

func TestObserveAllocator(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		first := register(t, ctx, s, testKey("reconcile-first"))
		second := register(t, ctx, s, testKey("reconcile-second"))

		now := time.Now()
		insertAllocatorMark(t, ctx, s, now.Add(-3*time.Hour), first.ID)
		insertAllocatorMark(t, ctx, s, now.Add(-2*time.Hour), second.ID)
		// Younger than the minimum age, so its position is no bound yet.
		insertAllocatorMark(t, ctx, s, now.Add(-30*time.Minute), second.ID)

		view, err := s.ObserveAllocator(ctx, time.Hour, true)
		require.NoError(t, err)
		require.False(t, view.Now.IsZero())
		require.Zero(t, view.DoneThrough)
		require.Equal(t, second.ID, view.Position)
		require.Equal(t, second.ID, view.Bound, "the newest eligible mark bounds the positions")
		require.EqualValues(t, 3, queryID(t, ctx, s, "SELECT COUNT(*) FROM graft_gc_allocator_marks"), "a dry run records nothing")

		view, err = s.ObserveAllocator(ctx, time.Hour, false)
		require.NoError(t, err)
		require.Equal(t, second.ID, view.Bound)
		require.EqualValues(t, 0, queryID(t, ctx, s,
			"SELECT COUNT(*) FROM graft_gc_allocator_marks WHERE observed_at = ?", timeValue(s, now.Add(-3*time.Hour))),
			"a mark older than the newest eligible one is dropped")
		require.EqualValues(t, 3, queryID(t, ctx, s, "SELECT COUNT(*) FROM graft_gc_allocator_marks"))

		// With no minimum age the mark of the first run is the newest eligible one,
		// so the marks older than it are dropped and only the two newest remain.
		_, err = s.ObserveAllocator(ctx, 0, false)
		require.NoError(t, err)
		require.EqualValues(t, 2, queryID(t, ctx, s, "SELECT COUNT(*) FROM graft_gc_allocator_marks"))

		// The allocator keeps counting past the marks, and the cursor is what
		// AdvanceReconcile leaves.
		register(t, ctx, s, testKey("reconcile-third"))
		view, err = s.ObserveAllocator(ctx, 0, true)
		require.NoError(t, err)
		require.Greater(t, view.Position, second.ID, "the allocator counts every created repository")

		require.NoError(t, s.AdvanceReconcile(ctx, second.ID))
		view, err = s.ObserveAllocator(ctx, 0, true)
		require.NoError(t, err)
		require.Equal(t, second.ID, view.DoneThrough)
		require.NoError(t, s.AdvanceReconcile(ctx, 0), "the cursor only moves forward")
		view, err = s.ObserveAllocator(ctx, 0, true)
		require.NoError(t, err)
		require.Equal(t, second.ID, view.DoneThrough)
	})

}

func TestUnrecordedIDsAndRecordLeak(t *testing.T) {
	forEachDialect(t, func(t *testing.T, ctx context.Context, s *sqlstore.Store) {
		first := register(t, ctx, s, testKey("reconcile-a"))
		second := register(t, ctx, s, testKey("reconcile-b"))

		ids, err := s.UnrecordedIDs(ctx, 0, 0)
		require.NoError(t, err)
		require.Empty(t, ids, "an empty range holds no unrecorded ID")
		ids, err = s.UnrecordedIDs(ctx, second.ID, first.ID)
		require.NoError(t, err)
		require.Empty(t, ids, "an inverted range holds no unrecorded ID")

		ids, err = s.UnrecordedIDs(ctx, 0, second.ID)
		require.NoError(t, err)
		require.Empty(t, ids, "every allocated ID has a row")

		// An ID whose row is gone without a tombstone is a leak: the deletion
		// bypassed DeleteRepository, so only reconciliation can find it.
		_, err = s.DeleteRepository(ctx, testKey("reconcile-b"))
		require.NoError(t, err)
		execSQL(t, ctx, s, "DELETE FROM graft_gc_removed WHERE repo_id = ?", second.ID)

		ids, err = s.UnrecordedIDs(ctx, first.ID, second.ID)
		require.NoError(t, err)
		require.Equal(t, []int64{second.ID}, ids)

		ids, err = s.UnrecordedIDs(ctx, 0, second.ID+2)
		require.NoError(t, err)
		require.Equal(t, []int64{second.ID, second.ID + 1, second.ID + 2}, ids,
			"the allocator may have handed out IDs whose row never appeared")

		// A living repository is never recorded as leaked.
		recorded, err := s.RecordLeak(ctx, first.ID)
		require.NoError(t, err)
		require.False(t, recorded)
		require.EqualValues(t, 0, queryID(t, ctx, s, "SELECT COUNT(*) FROM graft_gc_removed WHERE repo_id = ?", first.ID))

		recorded, err = s.RecordLeak(ctx, second.ID)
		require.NoError(t, err)
		require.True(t, recorded)
		recorded, err = s.RecordLeak(ctx, second.ID)
		require.NoError(t, err)
		require.False(t, recorded, "a recorded leak is not recorded again")

		ids, err = s.UnrecordedIDs(ctx, 0, second.ID)
		require.NoError(t, err)
		require.Empty(t, ids, "the tombstone records the ID")
	})
}
