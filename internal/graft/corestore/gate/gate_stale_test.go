package gate_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/graft/corestore/sqlstore"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

const (
	repositoryExistsMethod = "/gitaly.RepositoryService/RepositoryExists"
	createRepositoryMethod = "/gitaly.RepositoryService/CreateRepository"
	removeRepositoryMethod = "/gitaly.RepositoryService/RemoveRepository"
)

var errNotImported = structerr.NewFailedPrecondition("repository is not imported into the storage core")

// staleCopies returns the local repositories the gate moved into the
// temporary directory of the storage as stale caches.
func staleCopies(t *testing.T, cfg config.Cfg) []string {
	t.Helper()
	tmp, err := config.NewLocator(cfg).TempDir(cfg.Storages[0].Name)
	require.NoError(t, err)
	matches, err := filepath.Glob(filepath.Join(tmp, "graft-stale-*", "repo.git"))
	require.NoError(t, err)
	return matches
}

// purgeReadClosed starts cleanup on an archived repository and moves its
// observation deadline into the past, as waiting seven days does.
func purgeReadClosed(ctx context.Context, core *corestore.Core, key corestore.RepoKey) error {
	_, err := core.Meta.StartRepositoryCleanup(ctx, key, "admin", time.Nanosecond, 7*24*time.Hour)
	if err != nil {
		return err
	}
	store, ok := core.Meta.(*sqlstore.Store)
	if !ok {
		panic("gate tests expect an sqlstore-backed metadata store")
	}
	when := time.Now().UTC().Add(-time.Nanosecond)
	var value any = when.Format(time.RFC3339Nano)
	query := "UPDATE graft_repositories SET purge_after = ? WHERE storage = ? AND relative_path = ?"
	args := []any{value, key.Storage, key.RelativePath}
	if store.Dialect() == sqlstore.Postgres {
		query, args = "UPDATE graft_repositories SET purge_after = $1 WHERE storage = $2 AND relative_path = $3", []any{when, key.Storage, key.RelativePath}
	}
	_, err = store.DB().ExecContext(ctx, query, args...)
	return err
}

// registerHead registers repo in the database with a first changeset holding
// HEAD, as CreateRepository does.
func registerHead(t *testing.T, ctx context.Context, core *corestore.Core, repo *gitalypb.Repository) corestore.Repository {
	t.Helper()
	rec, _, err := core.Meta.RegisterRepository(ctx, corestore.KeyOf(repo), gittest.DefaultObjectHash.Format, corestore.InitialState{
		Changeset: &corestore.ChangesetInput{
			Updates:   []corestore.RefUpdate{{Name: "HEAD", Expect: corestore.ExpectValue, New: corestore.SymbolicTarget("refs/heads/main")}},
			Operation: "create_repository",
			NodeID:    core.NodeID,
		},
	})
	require.NoError(t, err)
	return rec
}

// existsHandler answers RepositoryExists from the local directory.
func existsHandler(path string) grpc.UnaryHandler {
	return func(context.Context, any) (any, error) {
		_, err := os.Stat(path)
		return &gitalypb.RepositoryExistsResponse{Exists: err == nil}, nil
	}
}

// A local repository with a watermark whose key the database does not have is
// the stale cache of a repository removed on another node: the gate moves it
// into the temporary directory and the handler answers as for a missing
// repository. A local repository without a (valid) watermark is untracked and
// kept, and a repository registered since the catch-up is caught up again.
func TestGateStaleCacheOnCatchUp(t *testing.T) {
	t.Parallel()

	t.Run("watermark without database row", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		fresh := &fakeFreshener{err: errNotImported}
		g, cfg, _ := newGateEnv(t, corestore.ModeAuthoritative, fresh)
		repo, path := untrackedRepo(t, ctx, cfg)
		require.NoError(t, gitops.WriteWatermark(path, corestore.Watermark{RepoID: 7, Seq: 3}))

		resp, err := g.UnaryInterceptor()(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: repositoryExistsMethod}, existsHandler(path))
		require.NoError(t, err)
		require.False(t, resp.(*gitalypb.RepositoryExistsResponse).GetExists())
		require.NoDirExists(t, path)
		require.Len(t, staleCopies(t, cfg), 1, "the stale cache is moved, not deleted in place")
		require.Equal(t, []string{repo.GetRelativePath()}, fresh.called())
	})

	t.Run("watermark without database row, streaming", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		g, cfg, _ := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{err: errNotImported})
		repo, path := untrackedRepo(t, ctx, cfg)
		require.NoError(t, gitops.WriteWatermark(path, corestore.Watermark{RepoID: 7, Seq: 3}))

		stream := &fakeStream{ctx: ctx, msg: &gitalypb.UpdateReferencesRequest{Repository: repo}}
		err := g.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: allowedStreamMutator},
			func(srv any, ss grpc.ServerStream) error {
				return ss.RecvMsg(&gitalypb.UpdateReferencesRequest{})
			})
		require.NoError(t, err, "the handler answers as for a missing repository")
		require.NoDirExists(t, path)
	})

	t.Run("no watermark", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		g, cfg, _ := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{err: errNotImported})
		repo, path := untrackedRepo(t, ctx, cfg)

		_, err := g.UnaryInterceptor()(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: repositoryExistsMethod}, existsHandler(path))
		testhelper.RequireGrpcError(t, errNotImported, err)
		require.DirExists(t, path)
		require.Empty(t, staleCopies(t, cfg))
	})

	t.Run("corrupt watermark", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		g, cfg, _ := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{err: errNotImported})
		repo, path := untrackedRepo(t, ctx, cfg)
		require.NoError(t, os.MkdirAll(filepath.Dir(gitops.WatermarkPath(path)), 0o755))
		require.NoError(t, os.WriteFile(gitops.WatermarkPath(path), []byte("not json"), 0o644))

		_, err := g.UnaryInterceptor()(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: repositoryExistsMethod}, existsHandler(path))
		testhelper.RequireGrpcError(t, errNotImported, err)
		require.DirExists(t, path, "a corrupt watermark is no proof that the storage core wrote the repository")
	})

	t.Run("registered meanwhile", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		fresh := &fakeFreshener{err: errNotImported}
		g, cfg, core := newGateEnv(t, corestore.ModeAuthoritative, fresh)
		repo, path := untrackedRepo(t, ctx, cfg)
		require.NoError(t, gitops.WriteWatermark(path, corestore.Watermark{RepoID: 7, Seq: 3}))
		registerHead(t, ctx, core, repo)

		_, err := g.UnaryInterceptor()(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: repositoryExistsMethod}, existsHandler(path))
		testhelper.RequireGrpcError(t, errNotImported, err)
		require.DirExists(t, path, "the cache of a tracked repository is left to the syncer")
		require.Equal(t, []string{repo.GetRelativePath(), repo.GetRelativePath()}, fresh.called(), "caught up once more")
	})

	t.Run("gone meanwhile", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		g, cfg, _ := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{err: errNotImported})
		repo := &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: gittest.NewRepositoryName(t)}

		resp, err := g.UnaryInterceptor()(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo},
			&grpc.UnaryServerInfo{FullMethod: repositoryExistsMethod}, func(context.Context, any) (any, error) {
				return &gitalypb.RepositoryExistsResponse{}, nil
			})
		require.NoError(t, err, "a stale cache moved away by a concurrent request is a missing repository")
		require.False(t, resp.(*gitalypb.RepositoryExistsResponse).GetExists())
	})
}

// CreateRepository moves a stale cache out of the way before its handler runs.
func TestGateCreateRepositoryMovesStaleCacheFirst(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fresh := &fakeFreshener{}
	g, cfg, _ := newGateEnv(t, corestore.ModeAuthoritative, fresh)
	repo, path := untrackedRepo(t, ctx, cfg)
	require.NoError(t, gitops.WriteWatermark(path, corestore.Watermark{RepoID: 7, Seq: 3}))

	stop := structerr.NewInternal("stop after the handler")
	_, err := g.UnaryInterceptor()(ctx, &gitalypb.CreateRepositoryRequest{Repository: repo},
		&grpc.UnaryServerInfo{FullMethod: createRepositoryMethod}, func(context.Context, any) (any, error) {
			require.NoDirExists(t, path, "the handler finds no repository")
			return nil, stop
		})
	testhelper.RequireGrpcError(t, stop, err)
	require.Len(t, staleCopies(t, cfg), 1)
	require.Empty(t, fresh.called(), "CreateRepository never catches up")

	// An untracked repository is not moved: the handler reports it.
	repo, path = untrackedRepo(t, ctx, cfg)
	_, err = g.UnaryInterceptor()(ctx, &gitalypb.CreateRepositoryRequest{Repository: repo},
		&grpc.UnaryServerInfo{FullMethod: createRepositoryMethod}, func(context.Context, any) (any, error) {
			require.DirExists(t, path)
			return nil, structerr.NewAlreadyExists("creating repository: repository exists already")
		})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.DirExists(t, path)
}

// A hydration that read the database before RemoveRepository deleted the row
// can rename its copy into place after the handler removed the directory. The
// copy is a stale cache and RemoveRepository moves it away.
// TestGatePurgeDropsReinstalledCache follows the stale-cache rule through the
// archive lifecycle: a local copy whose watermark names a purged repository is
// moved aside on the next access, even when a hydration recreated it after the
// purge (doc/graft/PLAN13.md). The repository is purged through the metadata
// store, as a super admin does once the archive retention has passed.
func TestGatePurgeDropsReinstalledCache(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	g, cfg, core := newGateEnv(t, corestore.ModeAuthoritative, &fakeFreshener{err: errNotImported})
	repo, path := untrackedRepo(t, ctx, cfg)
	rec := registerHead(t, ctx, core, repo)
	require.NoError(t, gitops.WriteWatermark(path, corestore.Watermark{RepoID: rec.ID, Seq: 1}))

	// The local copy goes away, the operator archives the repository and a
	// super admin purges it: the row and its history are gone.
	require.NoError(t, os.RemoveAll(path))
	_, err := core.Meta.ArchiveRepository(ctx, corestore.KeyOf(repo), "operator")
	require.NoError(t, err)
	require.NoError(t, purgeReadClosed(ctx, core, corestore.KeyOf(repo)))
	_, err = core.Meta.PurgeRepository(ctx, corestore.KeyOf(repo))
	require.NoError(t, err)
	_, err = core.Meta.GetRepository(ctx, corestore.KeyOf(repo))
	require.ErrorIs(t, err, corestore.ErrRepositoryNotFound)

	// A hydration that started before the purge lands afterwards: it writes a
	// watermark naming the purged row.
	_, hydrated := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		RelativePath:           repo.GetRelativePath(),
	})
	require.NoError(t, gitops.WriteWatermark(hydrated, corestore.Watermark{RepoID: rec.ID, Seq: 1}))

	resp, err := g.UnaryInterceptor()(ctx, &gitalypb.RepositoryExistsRequest{Repository: repo},
		&grpc.UnaryServerInfo{FullMethod: repositoryExistsMethod}, existsHandler(hydrated))
	require.NoError(t, err)
	require.False(t, resp.(*gitalypb.RepositoryExistsResponse).GetExists())
	require.NoDirExists(t, hydrated)
	require.Len(t, staleCopies(t, cfg), 1, "the stale cache is moved, not deleted in place")
}

// The database key is the relative path as given, so the gate only accepts
// relative paths in their cleaned form; and none inside Gitaly's data
// directory, where the storage core keeps untrusted copies that the stale-cache
// rule must never remove.
func TestGateRejectsNonCanonicalOrReservedRelativePath(t *testing.T) {
	t.Parallel()

	for _, rel := range []string{
		"grp//proj.git", "./grp/proj.git", "grp/proj.git/", "grp/../grp/proj.git", "grp/./proj.git",
		"+gitaly", "+gitaly/graft-untrusted/stale-1/repo.git", "+gitaly/tmp/hydrate-1/repo.git",
	} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			fresh := &fakeFreshener{}
			g := newGate(t, corestore.ModeAuthoritative, fresh)
			repo := repoAt(rel)

			for _, tc := range []struct {
				fullMethod string
				req        any
			}{
				{fullMethod: accessorMethod, req: &gitalypb.HasLocalBranchesRequest{Repository: repo}},
				{fullMethod: createRepositoryMethod, req: &gitalypb.CreateRepositoryRequest{Repository: repo}},
			} {
				_, err := g.UnaryInterceptor()(ctx, tc.req, &grpc.UnaryServerInfo{FullMethod: tc.fullMethod},
					func(context.Context, any) (any, error) {
						require.FailNow(t, "the handler must not run", tc.fullMethod)
						return nil, nil
					})
				require.Equal(t, codes.InvalidArgument, status.Code(err), "%s: %v", tc.fullMethod, err)
			}

			// A removal is refused before the path is looked at: the archive
			// replaces it whatever the path.
			_, err := g.UnaryInterceptor()(ctx, &gitalypb.RemoveRepositoryRequest{Repository: repo},
				&grpc.UnaryServerInfo{FullMethod: removeRepositoryMethod}, func(context.Context, any) (any, error) {
					require.FailNow(t, "the handler must not run", removeRepositoryMethod)
					return nil, nil
				})
			require.Equal(t, codes.FailedPrecondition, status.Code(err), "%s: %v", removeRepositoryMethod, err)

			stream := &fakeStream{ctx: ctx, msg: &gitalypb.UpdateReferencesRequest{Repository: repo}}
			err = g.StreamInterceptor()(nil, stream, &grpc.StreamServerInfo{FullMethod: allowedStreamMutator},
				func(srv any, ss grpc.ServerStream) error {
					return ss.RecvMsg(&gitalypb.UpdateReferencesRequest{})
				})
			require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
			require.Empty(t, fresh.called(), "nothing is caught up for a rejected path")
		})
	}
}

// TestGateStaleCacheAcrossNodes reproduces the gap between the gate and the
// syncer on two nodes sharing a database and a bucket: after a repository is
// purged on one node, the other node's cached copy is not reported as an
// untracked repository and does not block creating the repository again.
func TestGateStaleCacheAcrossNodes(t *testing.T) {
	t.Parallel()

	for _, dialect := range coretest.Dialects() {
		t.Run(string(dialect), func(t *testing.T) {
			t.Parallel()

			databaseURL := coretest.DatabaseURL(t, dialect)
			bucketURL := coretest.BucketURL(t, false)
			a := startNode(t, coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, BucketURL: bucketURL, NodeID: "node-a"}))
			b := startNode(t, coretest.Open(t, coretest.Options{Dialect: dialect, DatabaseURL: databaseURL, BucketURL: bucketURL, NodeID: "node-b"}))

			repoOn := func(n node, rel string) (*gitalypb.Repository, string) {
				return &gitalypb.Repository{StorageName: n.cfg.Storages[0].Name, RelativePath: rel}, filepath.Join(n.cfg.Storages[0].Path, rel)
			}
			exists := func(t *testing.T, n node, repo *gitalypb.Repository) bool {
				t.Helper()
				resp, err := gitalypb.NewRepositoryServiceClient(n.conn).RepositoryExists(n.ctx, &gitalypb.RepositoryExistsRequest{Repository: repo})
				require.NoError(t, err)
				return resp.GetExists()
			}
			// purge archives the repository and purges it through the metadata
			// store, as a super admin does once the archive retention has passed.
			purge := func(t *testing.T, n node, repo *gitalypb.Repository) {
				t.Helper()
				_, err := n.core.Meta.ArchiveRepository(n.ctx, corestore.KeyOf(repo), "operator")
				require.NoError(t, err)
				require.NoError(t, purgeReadClosed(n.ctx, n.core, corestore.KeyOf(repo)))
				_, err = n.core.Meta.PurgeRepository(n.ctx, corestore.KeyOf(repo))
				require.NoError(t, err)
			}
			// cacheOnB creates a repository with a branch on A, makes B hydrate it
			// and purges it on A.
			cacheOnB := func(t *testing.T) (*gitalypb.Repository, string, int64) {
				t.Helper()
				s := a.newSeed(t)
				repoB, pathB := repoOn(b, s.repo.GetRelativePath())
				_, err := gitalypb.NewRefServiceClient(b.conn).FindBranch(b.ctx, &gitalypb.FindBranchRequest{Repository: repoB, Name: []byte("main")})
				require.NoError(t, err)
				wm, ok, err := gitops.ReadWatermark(pathB)
				require.NoError(t, err)
				require.True(t, ok)

				purge(t, a, s.repo)
				require.DirExists(t, pathB, "B still has its cached copy")
				return repoB, pathB, wm.RepoID
			}

			t.Run("accessors see a missing repository", func(t *testing.T) {
				repoB, pathB, _ := cacheOnB(t)

				require.False(t, exists(t, b, repoB))
				require.NoDirExists(t, pathB)
				_, err := gitalypb.NewRefServiceClient(b.conn).FindBranch(b.ctx, &gitalypb.FindBranchRequest{Repository: repoB, Name: []byte("main")})
				require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
			})

			t.Run("the repository can be created again", func(t *testing.T) {
				repoB, pathB, oldID := cacheOnB(t)

				_, err := gitalypb.NewRepositoryServiceClient(b.conn).CreateRepository(b.ctx, &gitalypb.CreateRepositoryRequest{Repository: repoB})
				require.NoError(t, err)

				rec, err := b.core.Meta.GetRepository(b.ctx, corestore.KeyOf(repoB))
				require.NoError(t, err)
				require.NotEqual(t, oldID, rec.ID)
				wm, ok, err := gitops.ReadWatermark(pathB)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, rec.ID, wm.RepoID)
				b.requireSameRefs(t, repoB)
				require.NotContains(t, b.dbRefs(t, repoB), "refs/heads/main", "the new repository has nothing of the removed one")

				// A serves the new repository too.
				repoA, _ := repoOn(a, repoB.GetRelativePath())
				require.True(t, exists(t, a, repoA))
				a.requireSameRefs(t, repoA)
			})

			t.Run("created on A, removed on B", func(t *testing.T) {
				repoA, pathA := repoOn(a, gittest.NewRepositoryName(t))
				_, err := gitalypb.NewRepositoryServiceClient(a.conn).CreateRepository(a.ctx, &gitalypb.CreateRepositoryRequest{Repository: repoA})
				require.NoError(t, err)
				repoB, _ := repoOn(b, repoA.GetRelativePath())
				purge(t, b, repoB)
				require.DirExists(t, pathA)

				require.False(t, exists(t, a, repoA))
				require.NoDirExists(t, pathA)
				_, err = gitalypb.NewRepositoryServiceClient(a.conn).CreateRepository(a.ctx, &gitalypb.CreateRepositoryRequest{Repository: repoA})
				require.NoError(t, err)
				a.requireSameRefs(t, repoA)
			})
		})
	}
}
