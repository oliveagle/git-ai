package syncer_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// errFaultCall is the failure the faulty meta store reports.
var errFaultCall = errors.New("the database refused the call")

// faultMeta is a corestore.MetaStore that fails the calls the test names and
// delegates every other one to the store it wraps. It makes the paths of the
// syncer that report a failure of the database reachable without a database
// that fails on demand.
type faultMeta struct {
	corestore.MetaStore

	mu     sync.Mutex
	faults map[string]error
}

// faultTheMeta wraps the store of the node, so that the syncer, which reads
// core.Meta on every call, goes through the wrapper.
func faultTheMeta(e *env) *faultMeta {
	f := &faultMeta{MetaStore: e.core.Meta}
	e.core.Meta = f
	return f
}

// fail fails the call name with err until clear is called.
func (f *faultMeta) fail(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.faults == nil {
		f.faults = map[string]error{}
	}
	f.faults[name] = err
}

// clear stops failing the call name.
func (f *faultMeta) clear(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.faults, name)
}

// faultOf returns the failure of the call name, nil when it must go through.
func (f *faultMeta) faultOf(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.faults[name]
}

func (f *faultMeta) GetRepository(ctx context.Context, key corestore.RepoKey) (corestore.Repository, error) {
	if err := f.faultOf("GetRepository"); err != nil {
		return corestore.Repository{}, err
	}
	return f.MetaStore.GetRepository(ctx, key)
}

func (f *faultMeta) DeleteRepository(ctx context.Context, key corestore.RepoKey) (bool, error) {
	if err := f.faultOf("DeleteRepository"); err != nil {
		return false, err
	}
	return f.MetaStore.DeleteRepository(ctx, key)
}

func (f *faultMeta) NewestCheckpoint(ctx context.Context, repoID int64) (corestore.Checkpoint, bool, error) {
	if err := f.faultOf("NewestCheckpoint"); err != nil {
		return corestore.Checkpoint{}, false, err
	}
	return f.MetaStore.NewestCheckpoint(ctx, repoID)
}

func (f *faultMeta) SyncBoundary(ctx context.Context, repoID int64, age time.Duration) (uint64, error) {
	if err := f.faultOf("SyncBoundary"); err != nil {
		return 0, err
	}
	return f.MetaStore.SyncBoundary(ctx, repoID, age)
}

func (f *faultMeta) ListChangesets(ctx context.Context, repoID int64, after, upTo uint64) ([]corestore.Changeset, error) {
	if err := f.faultOf("ListChangesets"); err != nil {
		return nil, err
	}
	return f.MetaStore.ListChangesets(ctx, repoID, after, upTo)
}

func (f *faultMeta) CheckpointRefs(ctx context.Context, repoID int64, seq uint64) (map[string]corestore.Target, error) {
	if err := f.faultOf("CheckpointRefs"); err != nil {
		return nil, err
	}
	return f.MetaStore.CheckpointRefs(ctx, repoID, seq)
}

func (f *faultMeta) ReportApplied(ctx context.Context, repoID int64, nodeID string, seq uint64) error {
	if err := f.faultOf("ReportApplied"); err != nil {
		return err
	}
	return f.MetaStore.ReportApplied(ctx, repoID, nodeID, seq)
}

func (f *faultMeta) ListReviews(ctx context.Context, repoID int64, targetBranch string, includeClosed bool, limit int) ([]*gitalypb.Review, error) {
	if err := f.faultOf("ListReviews"); err != nil {
		return nil, err
	}
	return f.MetaStore.ListReviews(ctx, repoID, targetBranch, includeClosed, limit)
}

// TestSyncerReportsAFailingRecordRead covers the first read of the record of a
// repository: a database that fails it must make the sync fail, not serve a
// repository whose head is unknown.
func TestSyncerReportsAFailingRecordRead(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	a.register(t, a.key(localRelPath), src, c)

	faultTheMeta(a).fail("GetRepository", errFaultCall)

	err := a.sy.EnsureFresh(a.ctx, a.pbRepo(localRelPath))
	require.Equal(t, codes.Internal, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "get repository")
}

// TestSyncerReportsAFailingCheckpointRead covers the read of the newest
// checkpoint of a repository that has no local copy: a failure there must fail
// the round instead of hydrating from changesets that may already be collected.
func TestSyncerReportsAFailingCheckpointRead(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	a.register(t, a.key(localRelPath), src, c)

	b := a.secondNode(t, sqlstore.SQLite)
	faultTheMeta(b).fail("NewestCheckpoint", errFaultCall)

	err := b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath))
	require.Equal(t, codes.Internal, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "newest checkpoint")
}

// TestSyncerReportsAFailingChangesetRead covers the read of the changesets a
// hydration fetches: a database that fails it must fail the round.
func TestSyncerReportsAFailingChangesetRead(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	a.register(t, a.key(localRelPath), src, c)

	b := a.secondNode(t, sqlstore.SQLite)
	faultTheMeta(b).fail("ListChangesets", errFaultCall)

	err := b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath))
	require.Equal(t, codes.Internal, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "changesets")
}

// TestSyncerReportsAFailingBoundaryRead covers the read of the sync boundary of
// a repository that has a local copy at an older changeset: a failure there
// must fail the round, because the boundary decides whether the copy can be
// caught up at all.
func TestSyncerReportsAFailingBoundaryRead(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	repo := a.register(t, a.key(localRelPath), src, c)

	b := a.secondNode(t, sqlstore.SQLite)
	require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))

	c = a.advanceMain(t, repo.ID, src, c)
	faultTheMeta(b).fail("SyncBoundary", errFaultCall)

	err := b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath))
	require.Equal(t, codes.Internal, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "sync boundary")
}

// TestSyncerReportsAFailingCheckpointRefs covers a seed whose checkpoint
// references cannot be read: the seed job fails the round instead of
// installing a repository without its references.
func TestSyncerReportsAFailingCheckpointRefs(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	repo := a.register(t, a.key(localRelPath), src, c)
	a.checkpoint(t, repo.ID, src)

	b := a.secondNode(t, sqlstore.SQLite)
	faultTheMeta(b).fail("CheckpointRefs", errFaultCall)

	err := b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath))
	require.Equal(t, codes.Internal, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "references")
}

// TestSyncerSyncsWhenTheAppliedReportFails covers the report of an applied
// sequence: it is a metric of the database, so its failure must not fail a
// sync that is complete.
func TestSyncerSyncsWhenTheAppliedReportFails(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	repo := a.register(t, a.key(localRelPath), src, c)

	b := a.secondNode(t, sqlstore.SQLite)
	faultTheMeta(b).fail("ReportApplied", errFaultCall)

	require.NoError(t, b.sy.EnsureFresh(b.ctx, b.pbRepo(localRelPath)))
	b.verifySynced(t, repo)
}

// TestSyncerReportsAFailingReadOfARemovedRecord covers the second read of a
// key whose local cache has a watermark: a database that fails it must not let
// the cache be taken for a stale one and moved away.
func TestSyncerReportsAFailingReadOfARemovedRecord(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	repo := a.register(t, a.key(localRelPath), src, c)
	require.NoError(t, a.sy.EnsureFresh(a.ctx, a.pbRepo(localRelPath)))

	_, err := a.core.Meta.DeleteRepository(a.ctx, repo.Key)
	require.NoError(t, err)

	f := faultTheMeta(a)
	f.fail("GetRepository", errFaultCall)

	err = a.sy.EnsureFresh(a.ctx, a.pbRepo(localRelPath))
	require.Equal(t, codes.Internal, status.Code(err), "%v", err)
	require.ErrorContains(t, err, "get repository")
	_, statErr := os.Lstat(a.localPath())
	require.NoError(t, statErr, "the cache stays while whether the repository was removed is unknown")
}

// TestSyncerReportsAnUnreadableWatermark covers the watermark the applied
// sequence of a repository is read from: a file that cannot be read is an
// error of the caller, not a repository at changeset 0.
func TestSyncerReportsAnUnreadableWatermark(t *testing.T) {
	t.Parallel()

	a := newEnv(t, envOptions{})
	src := a.newSource(t)
	c := a.writeCommit(t, src)
	a.register(t, a.key(localRelPath), src, c)
	require.NoError(t, a.sy.EnsureFresh(a.ctx, a.pbRepo(localRelPath)))

	// A directory where the watermark file should be: reading it fails on
	// every platform, whatever the user the tests run as.
	require.NoError(t, os.Remove(gitops.WatermarkPath(a.localPath())))
	require.NoError(t, os.Mkdir(gitops.WatermarkPath(a.localPath()), 0o755))

	_, _, err := a.sy.Applied(a.ctx, a.pbRepo(localRelPath))
	require.Error(t, err)
	require.ErrorContains(t, err, "read watermark")
}
