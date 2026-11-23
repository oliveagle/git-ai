package derived

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// TestCommitsTreesAndShallowRepositories covers the reads the walk is built on:
// resolving and reading commits, finding the commit that last changed a path,
// reading one tree entry, and telling a shallow repository apart.
func TestCommitsTreesAndShallowRepositories(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repoProto, repoPath := createRepo(t, cfg)
	repo := localrepo.NewTestRepo(t, cfg, repoProto)

	ids := writeHistory(t, cfg, repoPath, []historyCommit{
		{name: "root", changes: []fileChange{{path: "a/x.txt", content: "one\n"}}},
		{name: "next", parents: []string{"root"}, changes: []fileChange{{path: "a/x.txt", content: "two\n"}}},
	})
	deriver := newTestDeriver(t, ctx, cfg, repo, false)

	commit, err := deriver.LastCommit(ctx, "next", "a/x.txt")
	require.NoError(t, err)
	require.Equal(t, ids["next"].String(), commit.GetId())

	// A path that no commit changed and a revision that does not exist are both
	// reported as not found, which callers tell apart from a failure.
	var notFound catfile.NotFoundError
	_, err = deriver.LastCommit(ctx, "next", "a/missing.txt")
	require.ErrorAs(t, err, &notFound)
	_, err = deriver.LastCommit(ctx, "no-such-branch", "a/x.txt")
	require.ErrorAs(t, err, &notFound)

	first, err := deriver.LastCommitOf(ctx, ids["root"], "a/x.txt")
	require.NoError(t, err)
	require.Equal(t, ids["root"].String(), first.GetId())

	resolved, err := deriver.ResolveCommit(ctx, "root")
	require.NoError(t, err)
	require.Equal(t, ids["root"], resolved)
	_, err = deriver.ResolveCommit(ctx, "no-such-branch")
	require.ErrorAs(t, err, &notFound)

	rootEntry, err := deriver.TreeEntry(ctx, ids["next"], ".")
	require.NoError(t, err)
	require.Equal(t, ".", string(rootEntry.GetPath()))
	require.EqualValues(t, treeMode, rootEntry.GetMode())

	fileEntry, err := deriver.TreeEntry(ctx, ids["next"], "a/x.txt")
	require.NoError(t, err)
	require.Equal(t, "a/x.txt", string(fileEntry.GetPath()))
	missing, err := deriver.TreeEntry(ctx, ids["next"], "a/none.txt")
	require.NoError(t, err)
	require.Nil(t, missing)

	shallow, err := deriver.Shallow(ctx)
	require.NoError(t, err)
	require.False(t, shallow)
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "shallow"), []byte(ids["root"].String()+"\n"), 0o644))
	shallow, err = deriver.Shallow(ctx)
	require.NoError(t, err)
	require.True(t, shallow)

	// A blob is not a tree: the walk refuses it instead of reading no entries,
	// and an object that is not there cannot be read at all.
	blob := gittest.WriteBlob(t, cfg, repoPath, []byte("x\n"))
	_, err = deriver.readTree(ctx, blob.String())
	require.ErrorContains(t, err, "not a tree")
	_, err = deriver.readTree(ctx, strings.Repeat("ab", 20))
	require.ErrorContains(t, err, "reading tree")

	object, err := deriver.reader.Object(ctx, git.Revision(blob.String()))
	require.NoError(t, err)
	fp, err := entryFingerprint(object, "x")
	require.NoError(t, err)
	require.Equal(t, fingerprint{}, fp)
}
