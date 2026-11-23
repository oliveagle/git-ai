package sqlstore

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// checkpointInput builds a valid checkpoint of the head of repoID: the
// statements that write it run, so a fault of one of them is reported.
func checkpointInput(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) corestore.CheckpointInput {
	t.Helper()

	refs, seq, err := store.Refs(ctx, repo.ID)
	require.NoError(t, err)
	require.NotEmpty(t, refs)
	require.Equal(t, repo.HeadSeq, seq)

	return corestore.CheckpointInput{
		Seq:          seq,
		Refs:         refs,
		PackKey:      corestore.NewPackKey(repo.ID, sha256.Sum256([]byte("an owned pack"))),
		PackBytes:    42,
		PackDeadline: time.Now().Add(time.Hour),
	}
}

// headRepository registers the repository of the tests and commits one
// changeset to it, so that it has a head sequence, a reference and a
// checkpoint can be created at that sequence.
func headRepository(t *testing.T, store *faultStore, ctx context.Context) corestore.Repository {
	t.Helper()

	repo := store.register(t, ctx)
	_, err := store.CommitChangeset(ctx, corestore.ChangesetInput{
		RepoID:    repo.ID,
		Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: "0000000000000000000000000000000000000001"}},
		Operation: "test",
		Actor:     "tester",
		NodeID:    "node-a",
	})
	require.NoError(t, err)

	repo, err = store.GetRepository(ctx, repo.Key)
	require.NoError(t, err)
	require.EqualValues(t, 1, repo.HeadSeq)
	return repo
}

// TestTheWritesOfTheStoreReportTheirFailures covers the write statements of the
// store one at a time. The test fails only the statement it names, so the reads
// that come first succeed and the error path of that write is the one reached.
func TestTheWritesOfTheStoreReportTheirFailures(t *testing.T) {
	key := corestore.RepoKey{Storage: "default", RelativePath: "@hashed/x.git"}

	for _, tc := range []struct {
		name  string
		fault string
		call  func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository)
	}{
		{
			name:  "the archived state of a repository",
			fault: "SET archived_at = ?",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				_, err := store.ArchiveRepository(ctx, key, "admin")
				require.ErrorContains(t, err, "archive the repository")
			},
		},
		{
			name:  "the closed reads of a cleanup",
			fault: "SET read_closed_at = ?, read_closed_by = ?",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				_, err := store.ArchiveRepository(ctx, key, "admin")
				require.NoError(t, err)

				_, err = store.StartRepositoryCleanup(ctx, key, "admin", time.Nanosecond, time.Hour)
				require.ErrorContains(t, err, "start the cleanup")
			},
		},
		{
			name:  "the reopened state of an unarchive",
			fault: "SET archived_at = NULL",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				_, err := store.ArchiveRepository(ctx, key, "admin")
				require.NoError(t, err)

				_, err = store.UnarchiveRepository(ctx, key)
				require.ErrorContains(t, err, "unarchive the repository")
			},
		},
		{
			name:  "the deleted row of a purge",
			fault: "DELETE FROM graft_repositories WHERE repo_id = ?",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, err := store.ArchiveRepository(ctx, key, "admin")
				require.NoError(t, err)
				_, err = store.StartRepositoryCleanup(ctx, key, "admin", time.Nanosecond, time.Nanosecond)
				require.NoError(t, err)
				// The observation is over before the purge, whatever the
				// granularity of the database clock.
				_, err = store.DB().ExecContext(ctx, store.bind("UPDATE graft_repositories SET purge_after = ? WHERE repo_id = ?"),
					store.timeArg(time.Now().Add(-time.Hour)), repo.ID)
				require.NoError(t, err)

				_, err = store.PurgeRepository(ctx, key)
				require.ErrorContains(t, err, "delete repository")
			},
		},
		{
			name:  "the row of a repository delete",
			fault: "DELETE FROM graft_repositories WHERE repo_id = ?",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				deleted, err := store.DeleteRepository(ctx, key)
				require.Error(t, err)
				require.False(t, deleted)
			},
		},
		{
			name:  "the checkpoint",
			fault: "INSERT INTO graft_checkpoints",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, _, err := store.CreateCheckpoint(ctx, repo.ID, checkpointInput(t, store, ctx, repo))
				require.ErrorContains(t, err, "insert checkpoint")
			},
		},
		{
			name:  "the references of a checkpoint",
			fault: "INSERT INTO graft_checkpoint_refs",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, _, err := store.CreateCheckpoint(ctx, repo.ID, checkpointInput(t, store, ctx, repo))
				require.ErrorContains(t, err, "insert checkpoint references")
			},
		},
		{
			name:  "the stamp of a checkpoint",
			fault: "UPDATE graft_checkpoints SET created_at = ?",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, _, err := store.CreateCheckpoint(ctx, repo.ID, checkpointInput(t, store, ctx, repo))
				require.ErrorContains(t, err, "stamp checkpoint")
			},
		},
		{
			name:  "the deleted checkpoint",
			fault: "DELETE FROM graft_checkpoints",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, _, err := store.CreateCheckpoint(ctx, repo.ID, checkpointInput(t, store, ctx, repo))
				require.NoError(t, err)

				_, err = store.DeleteCheckpoint(ctx, repo.ID, 1)
				require.ErrorContains(t, err, "delete checkpoint")
			},
		},
		{
			name:  "the head of a changeset",
			fault: "SET head_seq = ?, log_seq = ?",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, err := store.CommitChangeset(ctx, corestore.ChangesetInput{
					RepoID:    repo.ID,
					Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: "0000000000000000000000000000000000000001"}},
					Operation: "test",
					Actor:     "tester",
					NodeID:    "node-a",
				})
				require.Error(t, err)
			},
		},
		{
			name:  "the applied sequence of a node",
			fault: "INSERT INTO graft_nodes",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				require.Error(t, store.ReportApplied(ctx, repo.ID, "node-a", repo.HeadSeq))
			},
		},
		{
			name:  "the review",
			fault: "INSERT INTO graft_reviews",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, err := store.InsertReview(ctx, repo.ID, func(string) (*gitalypb.Review, error) {
					return &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t"), HeadCommitId: "abc"}, nil
				}, corestore.ReviewInsert{})
				require.Error(t, err)
			},
		},
		{
			name:  "the mirror target",
			fault: "INSERT INTO graft_mirror_targets",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				require.Error(t, store.RegisterTarget(ctx, faultMirrorKey(), faultMirrorTarget()))
			},
		},
		{
			name:  "the deleted mirror target",
			fault: "DELETE FROM graft_mirror_targets",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				require.NoError(t, store.RegisterTarget(ctx, faultMirrorKey(), faultMirrorTarget()))
				require.Error(t, store.UnregisterTarget(ctx, faultMirrorKey(), "gitee"))
			},
		},
		{
			name:  "the mirror sync",
			fault: "INSERT INTO graft_mirror_syncs",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				require.Error(t, store.RecordSync(ctx, faultMirrorKey(), faultMirrorSync()))
			},
		},
		{
			name:  "the maintenance lease",
			fault: "INSERT INTO graft_gc_leases",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, err := store.AcquireMaintenanceLease(ctx, repo.ID, "node-a", time.Minute)
				require.ErrorContains(t, err, "write maintenance lease")
			},
		},
		{
			name:  "the renewed maintenance lease",
			fault: "UPDATE graft_gc_leases SET expires_at",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				held, err := store.AcquireMaintenanceLease(ctx, repo.ID, "node-a", time.Minute)
				require.NoError(t, err)
				require.True(t, held)

				_, err = store.RenewMaintenanceLease(ctx, repo.ID, "node-a", time.Minute)
				require.Error(t, err)
			},
		},
		{
			name:  "the released maintenance lease",
			fault: "DELETE FROM graft_gc_leases",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				held, err := store.AcquireMaintenanceLease(ctx, repo.ID, "node-a", time.Minute)
				require.NoError(t, err)
				require.True(t, held)

				require.Error(t, store.ReleaseMaintenanceLease(ctx, repo.ID, "node-a"))
			},
		},
		{
			name:  "the observed orphan",
			fault: "INSERT INTO graft_gc_orphans",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				require.Error(t, store.RecordOrphans(ctx, repo.ID, []string{"pack-1"}))
			},
		},
		{
			name:  "the forgotten orphan",
			fault: "DELETE FROM graft_gc_orphans",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				require.NoError(t, store.RecordOrphans(ctx, repo.ID, []string{"pack-1"}))
				require.Error(t, store.ForgetOrphans(ctx, repo.ID, []string{"pack-1"}))
			},
		},
		{
			name:  "the observed allocator",
			fault: "INSERT INTO graft_gc_allocator_marks",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				_, err := store.ObserveAllocator(ctx, 0, false)
				require.ErrorContains(t, err, "record the allocator mark")
			},
		},
		{
			name:  "the recorded leak",
			fault: "INSERT INTO graft_gc_removed",
			call: func(t *testing.T, store *faultStore, ctx context.Context, repo corestore.Repository) {
				_, err := store.RecordLeak(ctx, repo.ID+10)
				require.ErrorContains(t, err, "record the tombstone")
			},
		},
		{
			name:  "the cursor of the reconciliation",
			fault: "UPDATE graft_gc_reconcile",
			call: func(t *testing.T, store *faultStore, ctx context.Context, _ corestore.Repository) {
				require.Error(t, store.AdvanceReconcile(ctx, 1))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, ctx := newFaultStore(t)
			repo := headRepository(t, store, ctx)

			store.fault(t, tc.fault)
			tc.call(t, store, ctx, repo)
		})
	}
}
