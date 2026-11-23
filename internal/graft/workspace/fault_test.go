package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// errFault is the failure the fault backend reports.
var errFault = errors.New("the server refused the call")

// faultBackend is a backend that fails the calls of the methods the test
// names. It stands for a server that is unreachable or refuses one request,
// and it lets the tests reach the error paths of the workspace.
type faultBackend struct {
	base *MemoryBackend
	// fail names the methods that fail.
	failResolve   bool
	failListTree  bool
	failObjects   bool
	failCommit    bool
	failLand      bool
	failDelete    bool
	failChangeset bool
}

func (b *faultBackend) ResolveRevision(ctx context.Context, rev string) (string, error) {
	if b.failResolve {
		return "", errFault
	}
	return b.base.ResolveRevision(ctx, rev)
}

func (b *faultBackend) ListTree(ctx context.Context, rev, path string, recursive bool) ([]Entry, error) {
	if b.failListTree {
		return nil, errFault
	}
	return b.base.ListTree(ctx, rev, path, recursive)
}

func (b *faultBackend) GetObjects(ctx context.Context, oids []string) (map[string][]byte, error) {
	if b.failObjects {
		return nil, errFault
	}
	return b.base.GetObjects(ctx, oids)
}

func (b *faultBackend) ReadFile(ctx context.Context, rev, path string) ([]byte, error) {
	return b.base.ReadFile(ctx, rev, path)
}

func (b *faultBackend) CommitFiles(ctx context.Context, opts CommitOptions) (string, error) {
	if b.failCommit {
		return "", errFault
	}
	return b.base.CommitFiles(ctx, opts)
}

func (b *faultBackend) Land(ctx context.Context, opts LandOptions) (LandResult, error) {
	if b.failLand {
		return LandResult{}, errFault
	}
	return b.base.Land(ctx, opts)
}

func (b *faultBackend) DeleteBranch(ctx context.Context, branch string) error {
	if b.failDelete {
		return errFault
	}
	return b.base.DeleteBranch(ctx, branch)
}

func (b *faultBackend) Changesets(ctx context.Context, after uint64, limit int) ([]Changeset, error) {
	if b.failChangeset {
		return nil, errFault
	}
	return b.base.Changesets(ctx, after, limit)
}

func (b *faultBackend) WatchChangesets(ctx context.Context, after uint64) (<-chan Changeset, <-chan error) {
	return b.base.WatchChangesets(ctx, after)
}

func (b *faultBackend) Blame(ctx context.Context, rev, path string) ([]BlameLine, error) {
	return b.base.Blame(ctx, rev, path)
}

func (b *faultBackend) PathHistory(ctx context.Context, rev, path string, limit int) ([]Commit, error) {
	return b.base.PathHistory(ctx, rev, path, limit)
}

// faultBackendFor returns a backend that serves main and fails the method the
// test names.
func faultBackendFor(t *testing.T, fail func(*faultBackend)) *faultBackend {
	t.Helper()

	base := NewMemoryBackend()
	base.Put("main", "a/x.txt", []byte("one"))
	b := &faultBackend{base: base}
	fail(b)
	return b
}

// TestCloneRefusesTheFailuresOfTheServer covers the clone that cannot reach
// the server, and the clone into a directory that cannot hold a workspace.
func TestCloneRefusesTheFailuresOfTheServer(t *testing.T) {
	t.Parallel()

	// A backend that cannot resolve the branch.
	b := faultBackendFor(t, func(b *faultBackend) { b.failResolve = true })
	_, err := Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), b, CloneOptions{Branch: "main"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolve branch")

	// A backend that cannot resolve the commit.
	_, err = Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), b, CloneOptions{At: "abc123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolve commit")

	// A backend that cannot list the tree.
	b = faultBackendFor(t, func(b *faultBackend) { b.failListTree = true })
	root := filepath.Join(t.TempDir(), "ws")
	_, err = Clone(context.Background(), root, b, CloneOptions{Branch: "main"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "the server refused the call")
	require.NoDirExists(t, root, "a clone that failed leaves nothing behind")
}

// TestCloneRefusesTheBadArguments covers the arguments of the clone that
// cannot be run.
func TestCloneRefusesTheBadArguments(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})

	_, err := Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), nil, CloneOptions{Branch: "main"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil backend")

	_, err = Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), b, CloneOptions{Branch: "main", At: "abc"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "either a branch or a commit")

	_, err = Clone(context.Background(), filepath.Join(t.TempDir(), "ws"), b, CloneOptions{Branch: "main", Sparse: []string{"../x"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "sparse prefix")

	// A directory that holds files already.
	root := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o644))
	_, err = Clone(context.Background(), root, b, CloneOptions{Branch: "main"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "is not empty")

	// A directory that cannot be read.
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	_, err = Clone(context.Background(), file, b, CloneOptions{Branch: "main"})
	require.Error(t, err)

	// A directory that cannot be created.
	_, err = Clone(context.Background(), filepath.Join(file, "ws"), b, CloneOptions{Branch: "main"})
	require.Error(t, err)
}

// TestOpenRefusesTheDirectoriesThatAreNoWorkspace covers the open of a
// directory that is not a workspace.
func TestOpenRefusesTheDirectoriesThatAreNoWorkspace(t *testing.T) {
	t.Parallel()

	_, err := Open(t.TempDir(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil backend")

	_, err = Open(t.TempDir(), NewMemoryBackend())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotWorkspace)
}

// TestFindRefusesTheDirectoriesThatAreNoWorkspace covers the search of a
// workspace from a directory that holds none.
func TestFindRefusesTheDirectoriesThatAreNoWorkspace(t *testing.T) {
	t.Parallel()

	_, err := Find(t.TempDir())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNotWorkspace)
}

// TestStatusRefusesAWorkspaceWithoutABaseCommit covers the workspace whose
// clone did not finish: every file would look new.
func TestStatusRefusesAWorkspaceWithoutABaseCommit(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(b *faultBackend) { b.failListTree = true })
	root := filepath.Join(t.TempDir(), "ws")
	_, err := Clone(context.Background(), root, b, CloneOptions{Branch: "main"})
	require.Error(t, err)

	// The clone left a state without a base commit behind.
	require.NoDirExists(t, root)
}

// TestCommitReportsTheFailureOfTheServer covers the commit that the server
// refuses.
func TestCommitReportsTheFailureOfTheServer(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(b *faultBackend) { b.failCommit = true })
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "the server refused the call")
}

// TestCommitRefusesTheEmptyMessage covers the commit without a message.
func TestCommitRefusesTheEmptyMessage(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "  "})
	require.Error(t, err)
	require.Contains(t, err.Error(), "commit message is required")
}

// TestCommitReportsTheFailureOfTheState covers the commit whose state cannot
// be saved.
func TestCommitReportsTheFailureOfTheState(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	// The metadata directory becomes a file, so the state cannot be written.
	require.NoError(t, os.RemoveAll(filepath.Join(ws.Root(), metaDirName)))
	require.NoError(t, os.WriteFile(filepath.Join(ws.Root(), metaDirName), []byte("x"), 0o644))

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
	require.Error(t, err)
}

// TestLandRefusesWithoutAPendingCommit covers the land of a workspace that has
// nothing to land.
func TestLandRefusesWithoutAPendingCommit(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)

	_, err := ws.Land(context.Background(), "main", "", Signature{Name: "dev", Email: "dev@example.com"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing to land")

	_, err = ws.DryRunLand(context.Background(), "main", "", Signature{Name: "dev", Email: "dev@example.com"}, time.Time{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing to land")
}

// TestLandRefusesAnUnknownStrategy covers the land with a strategy the server
// does not know.
func TestLandRefusesAnUnknownStrategy(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
	require.NoError(t, err)

	_, err = ws.Land(context.Background(), "main", "no-such-strategy", Signature{Name: "dev", Email: "dev@example.com"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "strategy")
}

// TestLandReportsTheFailureOfTheServer covers the land that the server
// refuses.
func TestLandReportsTheFailureOfTheServer(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
	require.NoError(t, err)

	b.failLand = true
	_, err = ws.Land(context.Background(), "main", "", Signature{Name: "dev", Email: "dev@example.com"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "the server refused the call")
}

// TestUpdateReportsTheFailureOfTheServer covers the update that cannot resolve
// the branch of the workspace.
func TestUpdateReportsTheFailureOfTheServer(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)

	b.failResolve = true
	_, err := ws.Update(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolve branch")
	require.Contains(t, err.Error(), "the server refused the call")
}

// TestUpdateRefusesAWorkspaceWithoutABranch covers the update of a workspace
// that is at a commit and not on a branch.
func TestUpdateRefusesAWorkspaceWithoutABranch(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)

	// The state on disk names no branch, so the workspace is at a commit.
	path := filepath.Join(ws.Root(), metaDirName, stateFileName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	raw["branch"] = ""
	patched, err := json.Marshal(raw)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, patched, 0o600))

	_, err = ws.Update(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing to update")
}

// TestSparseRefusesWhileTheCommitIsPending covers the change of the sparse set
// while a commit waits to be landed.
func TestSparseRefusesWhileTheCommitIsPending(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
	require.NoError(t, err)

	err = ws.Sparse(context.Background(), []string{"b"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "pending")
}

// TestRevertRefusesWhileTheCommitIsPending covers the revert of a workspace
// that has a commit waiting to be landed.
func TestRevertRefusesWhileTheCommitIsPending(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	_, err := ws.Commit(context.Background(), CommitRequest{Message: "change x"})
	require.NoError(t, err)

	err = ws.Revert(context.Background(), nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "pending")
}

// TestAbortRefusesWithoutAPendingCommit covers the abort of a workspace that
// has nothing to abort.
func TestAbortRefusesWithoutAPendingCommit(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)

	err := ws.Abort(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "nothing to abort")
}

// TestDiffReportsTheFileItCannotRead covers the diff of a file that the
// workspace cannot read any more.
func TestDiffReportsTheFileItCannotRead(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)
	writeWorkspaceFile(t, ws.Root(), "a/x.txt", "changed")

	// The file cannot be read any more.
	require.NoError(t, os.Chmod(filepath.Join(ws.Root(), "a", "x.txt"), 0o000))
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(ws.Root(), "a", "x.txt"), 0o644)
	})

	_, err := ws.Diff()
	require.Error(t, err)
}

// TestDiffShowsTheAddedAndTheDeletedFile covers the diff of a file that was
// added and of one that was deleted.
func TestDiffShowsTheAddedAndTheDeletedFile(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a/new.txt", "added\n")
	require.NoError(t, os.Remove(filepath.Join(ws.Root(), "a", "x.txt")))

	diff, err := ws.Diff()
	require.NoError(t, err)
	require.Contains(t, diff, "--- /dev/null")
	require.Contains(t, diff, "+++ /dev/null")
	require.Contains(t, diff, "+added")
}

// TestDiffReportsTheBinaryFile covers the diff of a file that is not text.
func TestDiffReportsTheBinaryFile(t *testing.T) {
	t.Parallel()

	b := faultBackendFor(t, func(*faultBackend) {})
	ws := cloneTestWorkspace(t, b)

	writeWorkspaceFile(t, ws.Root(), "a/bin.dat", "abc\x00def")

	diff, err := ws.Diff()
	require.NoError(t, err)
	require.Contains(t, diff, "Binary files differ")
}
