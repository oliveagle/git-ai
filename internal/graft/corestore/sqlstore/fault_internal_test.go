package sqlstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/mirrorstate"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// errFaultQuery is the failure the faulty driver reports.
var errFaultQuery = errors.New("the database refused the query")

// faultQuery names the queries the faulty driver fails: every query whose text
// holds it fails. The empty string fails nothing.
var faultQuery atomic.Value

// faultyDriver is the SQLite driver of the tests: it fails the queries the
// test names, so that the paths of the store that report a failure of the
// database are reached without a database that fails on demand.
type faultyDriver struct{}

func init() {
	sql.Register("sqlite3-faulty", faultyDriver{})
}

// Open opens a connection of the faulty driver.
func (faultyDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite3.SQLiteDriver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &faultyConn{Conn: conn}, nil
}

// faultyConn is a connection that fails the queries the test names.
type faultyConn struct {
	driver.Conn
}

// faultHits reports whether the query is one the test fails.
func faultHits(query string) bool {
	needle, _ := faultQuery.Load().(string)
	return needle != "" && strings.Contains(query, needle)
}

// Query fails the queries the test names.
func (c *faultyConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	if faultHits(query) {
		return nil, errFaultQuery
	}
	q, ok := c.Conn.(driver.Queryer)
	if !ok {
		return nil, fmt.Errorf("the connection cannot query")
	}
	return q.Query(query, args)
}

// Exec fails the queries the test names.
func (c *faultyConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if faultHits(query) {
		return nil, errFaultQuery
	}
	e, ok := c.Conn.(driver.Execer)
	if !ok {
		return nil, fmt.Errorf("the connection cannot execute")
	}
	return e.Exec(query, args)
}

// Prepare fails the statements of the queries the test names. The statements
// of a transaction are prepared, so this is where the writes fail.
func (c *faultyConn) Prepare(query string) (driver.Stmt, error) {
	if faultHits(query) {
		return nil, errFaultQuery
	}
	return c.Conn.Prepare(query)
}

// faultyStore opens a store whose database fails the queries the test names.
// The database is the file of the good store, so that the data of the setup is
// there.
func faultyStore(t *testing.T, target string) *Store {
	t.Helper()

	db, err := sql.Open("sqlite3-faulty", target+"?_busy_timeout=10000&_foreign_keys=on&_txlock=immediate")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	return &Store{db: db, readDB: db, dialect: SQLite, writers: &writeQueue{}}
}

// faultStore is a store whose database fails the queries of one table, with
// the data of a good store.
type faultStore struct {
	*Store
	target string
}

// newFaultStore opens a good store on a database in a temporary directory and
// returns it with the store that fails the queries of one table.
func newFaultStore(t *testing.T) (*faultStore, context.Context) {
	t.Helper()

	ctx := testhelper.Context(t)
	target := filepath.Join(t.TempDir(), "meta.db")
	good, err := Open(ctx, "sqlite://"+target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = good.Close() })

	return &faultStore{Store: faultyStore(t, target), target: target}, ctx
}

// fault fails the queries of one table until the test ends.
func (f *faultStore) fault(t *testing.T, table string) {
	t.Helper()

	faultQuery.Store(table)
	t.Cleanup(func() { faultQuery.Store("") })
}

// register registers a repository on the good database of the store.
func (f *faultStore) register(t *testing.T, ctx context.Context) corestore.Repository {
	t.Helper()

	good, err := Open(ctx, "sqlite://"+f.target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = good.Close() })

	repo, _, err := good.RegisterRepository(ctx, corestore.RepoKey{Storage: "default", RelativePath: "@hashed/x.git"}, "sha1", corestore.InitialState{})
	require.NoError(t, err)
	return repo
}

// TestTheStoreReportsTheFailuresOfItsQueries covers the paths of the store
// that report a failure of the database. The database fails the queries of one
// table, so that every query of that table is failed in turn.
func TestTheStoreReportsTheFailuresOfItsQueries(t *testing.T) {
	store, ctx := newFaultStore(t)
	repo := store.register(t, ctx)
	repoID := repo.ID

	for _, tc := range []struct {
		name  string
		table string
		call  func(ctx context.Context, s *Store) error
	}{
		{
			name:  "the repository of a key",
			table: "graft_repositories",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.GetRepository(ctx, corestore.RepoKey{Storage: "default", RelativePath: "@hashed/x.git"})
				return err
			},
		},
		{
			name:  "the repositories of a storage",
			table: "graft_repositories",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.ListRepositories(ctx, "default")
				return err
			},
		},
		{
			name:  "the references of a repository",
			table: "graft_refs",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.Refs(ctx, repoID)
				return err
			},
		},
		{
			name:  "the changesets of a repository",
			table: "graft_changesets",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.ListChangesets(ctx, repoID, 0, 100)
				return err
			},
		},
		{
			name:  "the changelog of a repository",
			table: "graft_changelog",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.ScanChangelog(ctx, repoID, 0, 10)
				return err
			},
		},
		{
			name:  "the reviews of a repository",
			table: "graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.ListReviews(ctx, repoID, "", false, 10)
				return err
			},
		},
		{
			name:  "one review of a repository",
			table: "graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.GetReview(ctx, repoID, "1")
				return err
			},
		},
		{
			name:  "the inserted review of a repository",
			table: "graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.InsertReview(ctx, repoID, func(id string) (*gitalypb.Review, error) {
					return &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t"), HeadCommitId: "abc"}, nil
				}, corestore.ReviewInsert{})
				return err
			},
		},
		{
			name:  "the updated review of a repository",
			table: "graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				return s.UpdateReview(ctx, repoID, "1", 1, &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t")}, false)
			},
		},
		{
			name:  "the checkpoints of a repository",
			table: "graft_checkpoints",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.CheckpointsBelow(ctx, repoID, 100)
				return err
			},
		},
		{
			name:  "the newest checkpoint of a repository",
			table: "graft_checkpoints",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.NewestCheckpoint(ctx, repoID)
				return err
			},
		},
		{
			name:  "the references of a checkpoint",
			table: "graft_checkpoints",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.CheckpointRefs(ctx, repoID, 1)
				return err
			},
		},
		{
			name:  "the snapshot of the garbage collector",
			table: "graft_gc_orphans",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.GCSnapshot(ctx, repoID)
				return err
			},
		},
		{
			name:  "the observed orphans of the garbage collector",
			table: "graft_gc_orphans",
			call: func(ctx context.Context, s *Store) error {
				return s.RecordOrphans(ctx, repoID, []string{"pack-1"})
			},
		},
		{
			name:  "the forgotten orphans of the garbage collector",
			table: "graft_gc_orphans",
			call: func(ctx context.Context, s *Store) error {
				return s.ForgetOrphans(ctx, repoID, []string{"pack-1"})
			},
		},
		{
			name:  "the registered mirror target",
			table: "graft_mirror_targets",
			call: func(ctx context.Context, s *Store) error {
				return s.RegisterTarget(ctx, faultMirrorKey(), faultMirrorTarget())
			},
		},
		{
			name:  "the mirror targets of a repository",
			table: "graft_mirror_targets",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.ListTargets(ctx, faultMirrorKey())
				return err
			},
		},
		{
			name:  "the recorded mirror sync",
			table: "graft_mirror_syncs",
			call: func(ctx context.Context, s *Store) error {
				return s.RecordSync(ctx, faultMirrorKey(), faultMirrorSync())
			},
		},
		{
			name:  "the mirror syncs of a repository",
			table: "graft_mirror_syncs",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.ListSyncs(ctx, faultMirrorKey())
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.fault(t, tc.table)
			require.Error(t, tc.call(ctx, store.Store))
		})
	}
}

// TestTheStoreReportsTheFailuresOfItsWrites covers the writes of the store
// that the database refuses.
func TestTheStoreReportsTheFailuresOfItsWrites(t *testing.T) {
	store, ctx := newFaultStore(t)
	repo := store.register(t, ctx)

	store.fault(t, "graft_repositories")
	_, _, err := store.RegisterRepository(ctx, corestore.RepoKey{Storage: "default", RelativePath: "@hashed/y.git"}, "sha1", corestore.InitialState{})
	require.Error(t, err)

	store.fault(t, "graft_changesets")
	_, err = store.CommitChangeset(ctx, corestore.ChangesetInput{
		RepoID:    repo.ID,
		Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: "0000000000000000000000000000000000000001"}},
		Operation: "test",
		Actor:     "tester",
		NodeID:    "node-a",
	})
	require.Error(t, err)
}

// TestTheStoreReportsTheFailuresOfTheRepositoryLifecycle covers the archive,
// the cleanup and the purge of a repository whose database fails.
func TestTheStoreReportsTheFailuresOfTheRepositoryLifecycle(t *testing.T) {
	store, ctx := newFaultStore(t)
	key := corestore.RepoKey{Storage: "default", RelativePath: "@hashed/x.git"}
	store.register(t, ctx)

	store.fault(t, "graft_repositories")
	_, err := store.ArchiveRepository(ctx, key, "admin")
	require.Error(t, err)

	_, err = store.StartRepositoryCleanup(ctx, key, "admin", 24*time.Hour, 7*24*time.Hour)
	require.Error(t, err)

	_, err = store.UnarchiveRepository(ctx, key)
	require.Error(t, err)

	_, err = store.ListArchivedRepositories(ctx, "default")
	require.Error(t, err)

	_, err = store.PurgeRepository(ctx, key)
	require.Error(t, err)
}

// TestTheStoreReportsTheFailuresOfTheMaintenance covers the maintenance lease
// and the removed repositories whose database fails.
func TestTheStoreReportsTheFailuresOfTheMaintenance(t *testing.T) {
	store, ctx := newFaultStore(t)
	repo := store.register(t, ctx)

	store.fault(t, "graft_gc_leases")
	_, err := store.AcquireMaintenanceLease(ctx, repo.ID, "node-a", time.Minute)
	require.Error(t, err)

	_, err = store.RenewMaintenanceLease(ctx, repo.ID, "node-a", time.Minute)
	require.Error(t, err)

	err = store.ReleaseMaintenanceLease(ctx, repo.ID, "node-a")
	require.Error(t, err)

	store.fault(t, "graft_gc_removed")
	_, err = store.ListRemovedRepositories(ctx, "default", time.Hour)
	require.Error(t, err)

	err = store.DeleteRemovedRepository(ctx, repo.ID)
	require.Error(t, err)
}

// TestTheStoreReportsTheFailuresOfTheReconciliation covers the reconciliation
// of the allocator whose database fails.
func TestTheStoreReportsTheFailuresOfTheReconciliation(t *testing.T) {
	store, ctx := newFaultStore(t)

	store.fault(t, "graft_gc_allocator_marks")
	_, err := store.ObserveAllocator(ctx, time.Hour, false)
	require.Error(t, err)

	store.fault(t, "graft_gc_removed")
	_, err = store.UnrecordedIDs(ctx, 0, 10)
	require.Error(t, err)

	store.fault(t, "graft_gc_reconcile")
	err = store.AdvanceReconcile(ctx, 10)
	require.Error(t, err)
}

// faultMirrorKey is the key of the repository whose mirror state the tests
// write.
func faultMirrorKey() mirrorstate.Key {
	return mirrorstate.Key{Storage: "default", RelativePath: "@hashed/x.git"}
}

// faultMirrorTarget is a mirror target of the repository.
func faultMirrorTarget() mirrorstate.Target {
	return mirrorstate.Target{Name: "gitee", URL: "https://example.invalid/x.git", Branches: []string{"main"}, Enabled: true}
}

// faultMirrorSync is the state of a publication of the mirror target.
func faultMirrorSync() mirrorstate.Sync {
	return mirrorstate.Sync{Target: "gitee", Status: "ok", Branches: map[string]string{"main": "0000000000000000000000000000000000000001"}}
}

// TestTheStoreReportsTheFailureOfEveryQuery covers the paths of the store
// that report the failure of one query. The database fails the query the test
// names, so that the second and the third query of a method are reached too.
func TestTheStoreReportsTheFailureOfEveryQuery(t *testing.T) {
	store, ctx := newFaultStore(t)
	repo := store.register(t, ctx)
	repoID := repo.ID
	key := corestore.RepoKey{Storage: "default", RelativePath: "@hashed/x.git"}

	for _, tc := range []struct {
		name  string
		query string
		call  func(ctx context.Context, s *Store) error
	}{
		{
			name:  "the lookup of a repository that is registered",
			query: "SELECT repo_id FROM graft_repositories WHERE storage",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.RegisterRepository(ctx, corestore.RepoKey{Storage: "default", RelativePath: "@hashed/y.git"}, "sha1", corestore.InitialState{})
				return err
			},
		},
		{
			name:  "the insert of a repository",
			query: "INSERT INTO graft_repositories",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.RegisterRepository(ctx, corestore.RepoKey{Storage: "default", RelativePath: "@hashed/y.git"}, "sha1", corestore.InitialState{})
				return err
			},
		},
		{
			name:  "the import of the reviews of a history",
			query: "INSERT INTO graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.ImportHistory(ctx, repoID, nil, []*gitalypb.Review{{Id: "1", TargetBranch: []byte("main")}})
				return err
			},
		},
		{
			name:  "the lookup of an imported review",
			query: "SELECT body, version FROM graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.ImportHistory(ctx, repoID, nil, []*gitalypb.Review{{Id: "1", TargetBranch: []byte("main")}})
				return err
			},
		},
		{
			name:  "the update of an imported review",
			query: "UPDATE graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				// The review exists already, so the import updates it.
				_, err := s.InsertReview(ctx, repoID, func(id string) (*gitalypb.Review, error) {
					return &gitalypb.Review{Id: "1", TargetBranch: []byte("main"), Title: []byte("t"), HeadCommitId: "abc"}, nil
				}, corestore.ReviewInsert{})
				require.NoError(t, err)
				_, _, err = s.ImportHistory(ctx, repoID, nil, []*gitalypb.Review{{
					Id:           "1",
					TargetBranch: []byte("main"),
					UpdatedAt:    timestamppb.New(time.Now().Add(time.Hour)),
				}})
				return err
			},
		},
		{
			name:  "the advance of the log sequence of a repository",
			query: "UPDATE graft_repositories SET log_seq",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.ImportHistory(ctx, repoID, nil, nil)
				return err
			},
		},
		{
			name:  "the delete of a repository",
			query: "DELETE FROM graft_repositories WHERE repo_id = ?",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.DeleteRepository(ctx, key)
				return err
			},
		},
		{
			name:  "the head sequence of a repository",
			query: "SELECT head_seq FROM graft_repositories",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.Refs(ctx, repoID)
				return err
			},
		},
		{
			name:  "the references of a repository",
			query: "SELECT name, target FROM graft_refs",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.Refs(ctx, repoID)
				return err
			},
		},
		{
			name:  "the applied sequence of a node",
			query: "INSERT INTO graft_nodes",
			call: func(ctx context.Context, s *Store) error {
				return s.ReportApplied(ctx, repoID, "node-a", 1)
			},
		},
		{
			name:  "the changelog of an imported history",
			query: "INSERT INTO graft_changelog",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.ImportHistory(ctx, repoID, []changelog.Entry{{
					Sequence:  1,
					Reference: "refs/heads/main",
					OldOID:    "0000000000000000000000000000000000000000",
					NewOID:    "0000000000000000000000000000000000000001",
					Operation: "test",
				}}, nil)
				return err
			},
		},
		{
			name:  "the identity of a land",
			query: "INSERT INTO graft_land_identities",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.ImportHistory(ctx, repoID, []changelog.Entry{{
					Sequence:  1,
					Reference: "refs/heads/main",
					OldOID:    "0000000000000000000000000000000000000000",
					NewOID:    "0000000000000000000000000000000000000001",
					Operation: "test",
					Lands: []changelog.Land{{
						Original: "0000000000000000000000000000000000000002",
						Landed:   "0000000000000000000000000000000000000001",
					}},
				}}, nil)
				return err
			},
		},
		{
			name:  "the recorded identity of a land",
			query: "SELECT log_seq, landed_commit FROM graft_land_identities",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.LookupLand(ctx, repoID, "refs/heads/main", "0000000000000000000000000000000000000001")
				return err
			},
		},
		{
			name:  "the count of the checkpoints of a repository",
			query: "SELECT COUNT(*) FROM graft_checkpoints",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.CheckpointsBelow(ctx, repoID, 100)
				return err
			},
		},
		{
			name:  "the references of a checkpoint",
			query: "SELECT name, target FROM graft_checkpoint_refs",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.CheckpointRefs(ctx, repoID, 1)
				return err
			},
		},
		{
			name:  "the delete of a checkpoint",
			query: "DELETE FROM graft_checkpoints WHERE repo_id = ? AND seq = ?",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.DeleteCheckpoint(ctx, repoID, 1)
				return err
			},
		},
		{
			name:  "the packs of the changesets of a repository",
			query: "SELECT pack_key, MAX(seq) FROM graft_changesets",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.GCSnapshot(ctx, repoID)
				return err
			},
		},
		{
			name:  "the observed orphans of a repository",
			query: "SELECT pack_key, first_seen FROM graft_gc_orphans",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.GCSnapshot(ctx, repoID)
				return err
			},
		},
		{
			name:  "the forgotten orphans of a repository",
			query: "DELETE FROM graft_gc_orphans",
			call: func(ctx context.Context, s *Store) error {
				return s.ForgetOrphans(ctx, repoID, []string{"pack-1"})
			},
		},
		{
			name:  "the archive of a repository",
			query: "UPDATE graft_repositories SET archived_at = ?",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.ArchiveRepository(ctx, key, "admin")
				return err
			},
		},
		{
			name:  "the cleanup of a repository",
			query: "UPDATE graft_repositories SET read_closed_at",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.StartRepositoryCleanup(ctx, key, "admin", 24*time.Hour, 7*24*time.Hour)
				return err
			},
		},
		{
			name:  "the unarchive of a repository",
			query: "UPDATE graft_repositories SET archived_at = NULL",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.UnarchiveRepository(ctx, key)
				return err
			},
		},
		{
			name:  "the lease of a repository",
			query: "SELECT holder, expires_at FROM graft_gc_leases",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.AcquireMaintenanceLease(ctx, repoID, "node-a", time.Minute)
				return err
			},
		},
		{
			name:  "the renewal of a lease",
			query: "UPDATE graft_gc_leases SET expires_at",
			call: func(ctx context.Context, s *Store) error {
				// The lease is held, so that the renewal updates it.
				held, err := s.AcquireMaintenanceLease(ctx, repoID, "node-a", time.Minute)
				require.NoError(t, err)
				require.True(t, held)
				_, err = s.RenewMaintenanceLease(ctx, repoID, "node-a", time.Minute)
				return err
			},
		},
		{
			name:  "the release of a lease",
			query: "DELETE FROM graft_gc_leases",
			call: func(ctx context.Context, s *Store) error {
				return s.ReleaseMaintenanceLease(ctx, repoID, "node-a")
			},
		},
		{
			name:  "the removed repositories of a storage",
			query: "FROM graft_gc_removed",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.ListRemovedRepositories(ctx, "default", time.Hour)
				return err
			},
		},
		{
			name:  "the delete of a removed repository",
			query: "DELETE FROM graft_gc_removed",
			call: func(ctx context.Context, s *Store) error {
				return s.DeleteRemovedRepository(ctx, repoID)
			},
		},
		{
			name:  "the unregister of a mirror target",
			query: "DELETE FROM graft_mirror_targets",
			call: func(ctx context.Context, s *Store) error {
				return s.UnregisterTarget(ctx, faultMirrorKey(), "gitee")
			},
		},
		{
			name:  "the count of the open reviews of a repository",
			query: "SELECT COUNT(*) FROM graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.InsertReview(ctx, repoID, func(id string) (*gitalypb.Review, error) {
					return &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t"), HeadCommitId: "abc"}, nil
				}, corestore.ReviewInsert{MaxOpen: 1})
				return err
			},
		},
		{
			name:  "the insert of a review",
			query: "INSERT INTO graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.InsertReview(ctx, repoID, func(id string) (*gitalypb.Review, error) {
					return &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t"), HeadCommitId: "abc"}, nil
				}, corestore.ReviewInsert{})
				return err
			},
		},
		{
			name:  "the advance of the review ID of a repository",
			query: "UPDATE graft_repositories SET next_review_id",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.InsertReview(ctx, repoID, func(id string) (*gitalypb.Review, error) {
					return &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t"), HeadCommitId: "abc"}, nil
				}, corestore.ReviewInsert{})
				return err
			},
		},
		{
			name:  "the body of a review",
			query: "SELECT body, version FROM graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.GetReview(ctx, repoID, "1")
				return err
			},
		},
		{
			name:  "the version of a review",
			query: "SELECT version FROM graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				return s.UpdateReview(ctx, repoID, "1", 1, &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t")}, false)
			},
		},
		{
			name:  "the update of a review",
			query: "UPDATE graft_reviews",
			call: func(ctx context.Context, s *Store) error {
				return s.UpdateReview(ctx, repoID, "1", 1, &gitalypb.Review{TargetBranch: []byte("main"), Title: []byte("t")}, false)
			},
		},
		{
			name:  "the delete of a reference",
			query: "DELETE FROM graft_refs WHERE repo_id = ? AND name = ?",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
					RepoID: repoID,
					Updates: []corestore.RefUpdate{{
						Name:   "refs/heads/main",
						Expect: corestore.ExpectValue,
						Old:    "0000000000000000000000000000000000000001",
					}},
					Operation: "test",
					Actor:     "tester",
					NodeID:    "node-a",
				})
				return err
			},
		},
		{
			name:  "the conflicting names of a changeset",
			query: "SELECT name, target FROM graft_refs WHERE repo_id = ? AND name IN (",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
					RepoID:    repoID,
					Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: "0000000000000000000000000000000000000001"}},
					Operation: "test",
					Actor:     "tester",
					NodeID:    "node-a",
				})
				return err
			},
		},
		{
			name:  "the head sequence of a committed changeset",
			query: "UPDATE graft_repositories SET head_seq",
			call: func(ctx context.Context, s *Store) error {
				_, err := s.CommitChangeset(ctx, corestore.ChangesetInput{
					RepoID:    repoID,
					Updates:   []corestore.RefUpdate{{Name: "refs/heads/main", Expect: corestore.ExpectValue, New: "0000000000000000000000000000000000000001"}},
					Operation: "test",
					Actor:     "tester",
					NodeID:    "node-a",
				})
				return err
			},
		},
		{
			name:  "the conflicting reference of a name",
			query: "SELECT name, target FROM graft_refs WHERE repo_id = ? AND name IN (",
			call: func(ctx context.Context, s *Store) error {
				_, _, err := s.ConflictingRef(ctx, repoID, "refs/heads/main")
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
