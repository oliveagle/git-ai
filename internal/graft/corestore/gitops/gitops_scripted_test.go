package gitops_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// scriptedFixture is newFixture with a git binary that runs the body of the
// script instead of git itself, delegating to the real git wherever the body
// does not return. It is how the operations are tested against a git that
// fails, or answers something git never would.
func scriptedFixture(t *testing.T, format string, body func(realGit string) string) fixture {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	protoRepo, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		ObjectFormat:           format,
		SkipCreationViaService: true,
	})

	realGit := gittest.NewCommandFactory(t, cfg).GetExecutionEnvironment(ctx).BinaryPath
	script := filepath.Join(testhelper.TempDir(t), "git")
	testhelper.WriteExecutable(t, script, []byte("#!/bin/sh\n"+body(realGit)+"\nexec "+fmt.Sprintf("%q", realGit)+" \"$@\"\n"))

	hash, err := git.ObjectHashByFormat(format)
	require.NoError(t, err)

	return fixture{
		ctx:  ctx,
		cfg:  cfg,
		repo: localrepo.NewTestRepo(t, cfg, protoRepo, gitcmd.WithGitBinaryPath(script)),
		path: path,
		hash: hash,
	}
}

// brokenSubcommand is a body that returns when git is asked for subcommand:
// acting is the shell that answers instead of git.
func brokenSubcommand(subcommand, acting string) func(string) string {
	return func(string) string {
		return fmt.Sprintf("for arg in \"$@\"; do\n\tif [ \"$arg\" = %q ]; then\n%s\n\tfi\ndone", subcommand, acting)
	}
}

func TestMissingObjectsReportsACatFileFailure(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("cat-file", "\t\techo boom >&2\n\t\texit 1"))

	missing, err := gitops.MissingObjects(f.ctx, f.repo, []git.ObjectID{f.commit(t, nil, "a")})
	require.ErrorContains(t, err, "cat-file")
	require.Empty(t, missing)
}

func TestMissingObjectsRejectsAnAnswerCountThatDoesNotMatch(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("cat-file", "\t\tprintf 'deadbeef blob\\n'\n\t\texit 0"))

	missing, err := gitops.MissingObjects(f.ctx, f.repo, []git.ObjectID{f.commit(t, nil, "a"), f.commit(t, nil, "b")})
	require.ErrorContains(t, err, "answers for 2 objects")
	require.Empty(t, missing)
}

func TestMissingObjectsRejectsAnAnswerForAnotherObject(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("cat-file", "\t\tprintf 'deadbeef blob\\ndeadbeef blob\\n'\n\t\texit 0"))

	missing, err := gitops.MissingObjects(f.ctx, f.repo, []git.ObjectID{f.commit(t, nil, "a"), f.commit(t, nil, "b")})
	require.ErrorContains(t, err, "unexpected answer")
	require.Empty(t, missing)
}

func TestBuildPackReportsAFailingPackObjects(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("pack-objects", "\t\techo broken >&2\n\t\texit 1"))

	_, err := gitops.BuildPack(f.ctx, f.repo, []git.ObjectID{f.commit(t, nil, "content")}, t.TempDir())
	require.ErrorContains(t, err, "pack-objects")
}

// The object list of rev-list has been read and packed, but rev-list then
// exits with an error: the pack is not trusted and the error is reported.
func TestBuildPackReportsARevListThatFailsAfterItsList(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(realGit string) string {
		return brokenSubcommand("rev-list", "\t\t"+fmt.Sprintf("%q", realGit)+" \"$@\"\n\t\texit 1")(realGit)
	})

	_, err := gitops.BuildPack(f.ctx, f.repo, []git.ObjectID{f.commit(t, nil, "content")}, t.TempDir())
	require.ErrorContains(t, err, "rev-list")
}

func TestBuildPackRejectsAPackTooShortForItsHeader(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("pack-objects", "\t\tcat >/dev/null\n\t\tprintf 'nope'\n\t\texit 0"))

	_, err := gitops.BuildPack(f.ctx, f.repo, []git.ObjectID{f.commit(t, nil, "content")}, t.TempDir())
	require.ErrorContains(t, err, "read pack header")
}

func TestBuildPackRejectsAPackWithoutAHeader(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("pack-objects", "\t\tcat >/dev/null\n\t\tprintf 'xxxxxxxxxxxxxxxxxxxx'\n\t\texit 0"))

	_, err := gitops.BuildPack(f.ctx, f.repo, []git.ObjectID{f.commit(t, nil, "content")}, t.TempDir())
	require.ErrorContains(t, err, "no pack header")
}

func TestIndexPackReportsAFailure(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("index-pack", "\t\techo broken >&2\n\t\texit 1"))

	err := gitops.IndexPack(f.ctx, f.repo, strings.NewReader("not a pack"))
	require.ErrorContains(t, err, "index pack")
}

func TestReadRefsReportsAForEachRefFailure(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("for-each-ref", "\t\techo broken >&2\n\t\texit 1"))

	refs, err := gitops.ReadRefs(f.ctx, f.repo)
	require.ErrorContains(t, err, "for-each-ref")
	require.Empty(t, refs)
}

// symbolic-ref answering without a final newline is not an answer.
func TestReadRefsReportsASymbolicRefItCannotRead(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("symbolic-ref", "\t\tprintf 'refs/heads/main'\n\t\texit 0"))

	refs, err := gitops.ReadRefs(f.ctx, f.repo)
	require.ErrorContains(t, err, "unexpected output")
	require.Empty(t, refs)
}

func TestReadRefsRejectsAMalformedReferenceLine(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("for-each-ref", "\t\tprintf 'one two three\\n'\n\t\texit 0"))

	refs, err := gitops.ReadRefs(f.ctx, f.repo)
	require.ErrorContains(t, err, "unexpected reference format")
	require.Empty(t, refs)
}

func TestReadRefsRejectsASymbolicReferenceWithAnInvalidName(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("for-each-ref",
		"\t\tprintf 'refs/heads/bad..name\\000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\\000refs/heads/main\\n'\n\t\texit 0"))

	refs, err := gitops.ReadRefs(f.ctx, f.repo)
	require.ErrorContains(t, err, "symbolic reference")
	require.Empty(t, refs)
}

// for-each-ref read a symbolic reference whose target is no longer symbolic by
// the time symbolic-ref is asked: what for-each-ref read is reported.
func TestReadRefsReportsASymbolicTargetThatStoppedBeingOne(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(string) string {
		return strings.Join([]string{
			`for arg in "$@"; do`,
			`	if [ "$arg" = "symbolic-ref" ]; then exit 1; fi`,
			`	if [ "$arg" = "for-each-ref" ]; then`,
			`		printf 'refs/remotes/o/HEAD\000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\000refs/heads/main\n'`,
			`		exit 0`,
			`	fi`,
			`done`,
		}, "\n")
	})

	refs, err := gitops.ReadRefs(f.ctx, f.repo)
	require.NoError(t, err)
	require.Equal(t, "ref:refs/heads/main", string(refs["refs/remotes/o/HEAD"]))
}

func TestReadHeadReportsARevParseFailure(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(string) string {
		return strings.Join([]string{
			`for arg in "$@"; do`,
			`	if [ "$arg" = "symbolic-ref" ]; then exit 1; fi`,
			`done`,
			`for arg in "$@"; do`,
			`	if [ "$arg" = "HEAD" ]; then echo broken >&2; exit 3; fi`,
			`done`,
		}, "\n")
	})

	head, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"HEAD"})
	require.ErrorContains(t, err, "rev-parse HEAD")
	require.Empty(t, head)
}

func TestReadHeadRejectsAnAnswerWithoutANewline(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(string) string {
		return strings.Join([]string{
			`for arg in "$@"; do`,
			`	if [ "$arg" = "symbolic-ref" ]; then exit 1; fi`,
			`done`,
			`last=`,
			`for arg in "$@"; do last=$arg; done`,
			`if [ "$last" = "HEAD" ]; then printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'; exit 0; fi`,
		}, "\n")
	})

	head, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"HEAD"})
	require.ErrorContains(t, err, "unexpected output for HEAD")
	require.Empty(t, head)
}

func TestPackOpenReportsAMissingFile(t *testing.T) {
	t.Parallel()

	pack := gitops.Pack{Path: filepath.Join(t.TempDir(), "gone.pack")}
	_, err := pack.Open()
	require.ErrorContains(t, err, "no such file")
}

func TestPackRemoveReportsAFileItCannotRemove(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o600))

	pack := gitops.Pack{Path: dir}
	require.Error(t, pack.Remove())
}

// A name that cannot be a reference is never put on a command line.
func TestReadRefValuesSkipsNamesThatCannotBeReferences(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")
	commit := f.commit(t, nil, "content")
	f.git(t, "update-ref", "refs/heads/a", commit.String())

	values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{
		"HEAD", "refs/heads/a", "refs/heads/bad..name", "main", "",
	})
	require.NoError(t, err)
	require.Equal(t, commit.String(), string(values["refs/heads/a"]))
	require.NotContains(t, values, "refs/heads/bad..name")
	require.NotContains(t, values, "main")
	require.NotContains(t, values, "")
}

func TestLocalNameConflictsReportsAParentAndAFirstReferenceBelow(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")
	commit := f.commit(t, nil, "content")
	f.git(t, "update-ref", "refs/heads/a", commit.String())
	f.git(t, "update-ref", "refs/heads/x/y", commit.String())

	conflicts, err := gitops.LocalNameConflicts(f.ctx, f.repo, []string{
		"refs/heads/a/b", "refs/heads/x", "junk", "refs/heads/bad..name", "refs/heads/a",
	})
	require.NoError(t, err)
	require.Equal(t, gitops.LocalRef{Name: "refs/heads/a", Target: corestore.Target(commit.String())}, conflicts["refs/heads/a/b"])
	require.Equal(t, gitops.LocalRef{Name: "refs/heads/x/y", Target: corestore.Target(commit.String())}, conflicts["refs/heads/x"])
	require.NotContains(t, conflicts, "refs/heads/a")
	require.Len(t, conflicts, 2)
}

func TestLocalNameConflictsIgnoresNamesThatCannotBeReferences(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")

	conflicts, err := gitops.LocalNameConflicts(f.ctx, f.repo, []string{"main", "refs/heads/bad..name"})
	require.NoError(t, err)
	require.Empty(t, conflicts)
}

func TestReadRefValuesReportsACatFileFailureOnDanglingNames(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("cat-file", "\t\techo boom >&2\n\t\texit 1"))

	values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/gone"})
	require.ErrorContains(t, err, "cat-file")
	require.Empty(t, values)
}

// Only the warnings about a name that was asked for are read; anything else
// on stderr is ignored.
func TestReadRefValuesIgnoresWarningsAboutOtherNames(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("cat-file", strings.Join([]string{
		"\t\tprintf 'warning: ignoring dangling symref refs/heads/other\\nsome noise\\n' >&2",
		"\t\texit 0",
	}, "\n")))

	values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/gone"})
	require.NoError(t, err)
	require.Empty(t, values)
}

func TestReadRefValuesReportsASymbolicRefFailureOnADanglingName(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(string) string {
		return strings.Join([]string{
			`for arg in "$@"; do`,
			`	if [ "$arg" = "symbolic-ref" ]; then echo broken >&2; exit 3; fi`,
			`	if [ "$arg" = "cat-file" ]; then`,
			`		printf 'warning: ignoring dangling symref refs/heads/gone\n' >&2`,
			`		exit 0`,
			`	fi`,
			`done`,
		}, "\n")
	})

	values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/gone"})
	require.ErrorContains(t, err, "symbolic-ref")
	require.Empty(t, values)
}

func TestReadRefValuesReportsAForEachRefFailure(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("for-each-ref", "\t\techo broken >&2\n\t\texit 1"))

	values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/a"})
	require.ErrorContains(t, err, "for-each-ref")
	require.Empty(t, values)
}

func TestLocalNameConflictsReportsAParentReadFailure(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", brokenSubcommand("for-each-ref", "\t\techo broken >&2\n\t\texit 1"))

	conflicts, err := gitops.LocalNameConflicts(f.ctx, f.repo, []string{"refs/heads/a/b"})
	require.ErrorContains(t, err, "for-each-ref")
	require.Empty(t, conflicts)
}

// The parent names are read first; the patterns of the names themselves are
// read after, and a failure there is reported as well.
func TestLocalNameConflictsReportsABelowReadFailure(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(string) string {
		return strings.Join([]string{
			`for arg in "$@"; do`,
			`	if [ "$arg" = "for-each-ref" ]; then`,
			`		for a in "$@"; do`,
			`			if [ "$a" = "refs/heads/x/y" ]; then echo broken >&2; exit 1; fi`,
			`		done`,
			`		exit 0`,
			`	fi`,
			`done`,
		}, "\n")
	})

	conflicts, err := gitops.LocalNameConflicts(f.ctx, f.repo, []string{"refs/heads/x/y"})
	require.ErrorContains(t, err, "for-each-ref")
	require.Empty(t, conflicts)
}

func TestReadRefsReportsASymbolicRefFailure(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(string) string {
		return strings.Join([]string{
			`for arg in "$@"; do`,
			`	if [ "$arg" = "symbolic-ref" ]; then echo broken >&2; exit 3; fi`,
			`	if [ "$arg" = "for-each-ref" ]; then`,
			`		printf 'refs/remotes/o/HEAD\000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\000refs/heads/main\n'`,
			`		exit 0`,
			`	fi`,
			`done`,
		}, "\n")
	})

	refs, err := gitops.ReadRefs(f.ctx, f.repo)
	require.ErrorContains(t, err, "symbolic-ref")
	require.Empty(t, refs)
}

func TestReadHeadRejectsAnAnswerThatIsNotAnObjectID(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(string) string {
		return strings.Join([]string{
			`for arg in "$@"; do`,
			`	if [ "$arg" = "symbolic-ref" ]; then exit 1; fi`,
			`done`,
			`last=`,
			`for arg in "$@"; do last=$arg; done`,
			`if [ "$last" = "HEAD" ]; then printf 'not-an-object-id\n'; exit 0; fi`,
		}, "\n")
	})

	head, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"HEAD"})
	require.ErrorContains(t, err, "HEAD:")
	require.Empty(t, head)
}

// Two references that cannot share a transaction and both create: there is
// nothing to delete first, so the conflict itself is reported.
func TestApplyRefsReportsAConflictBetweenTwoCreations(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")
	commit := f.commit(t, nil, "content")

	err := gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
		"refs/heads/a":   corestore.Target(commit.String()),
		"refs/heads/a/b": corestore.Target(commit.String()),
	})
	require.Error(t, err)
}

// A name warned about twice is read once.
func TestReadRefValuesReadsADanglingNameOnce(t *testing.T) {
	t.Parallel()

	f := scriptedFixture(t, "sha1", func(string) string {
		return strings.Join([]string{
			`for arg in "$@"; do`,
			`	if [ "$arg" = "cat-file" ]; then`,
			`		printf 'warning: ignoring dangling symref refs/heads/gone\n' >&2`,
			`		printf 'warning: ignoring dangling symref refs/heads/gone\n' >&2`,
			`		exit 0`,
			`	fi`,
			`	if [ "$arg" = "symbolic-ref" ]; then printf 'refs/heads/main\n'; exit 0; fi`,
			`done`,
		}, "\n")
	})

	values, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/gone"})
	require.NoError(t, err)
	require.Equal(t, "ref:refs/heads/main", string(values["refs/heads/gone"]))
}
