package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMemoryBackendCommitFilesRefusesTheActionsItCannotApply covers the
// actions the memory backend refuses, in the order the server applies them.
func TestMemoryBackendCommitFilesRefusesTheActionsItCannotApply(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	seeded := func(t *testing.T) *MemoryBackend {
		t.Helper()
		m := NewMemoryBackend()
		m.Put("main", "a/x.txt", []byte("one"))
		m.Put("main", "top.txt", []byte("top"))
		return m
	}

	t.Run("without a branch", func(t *testing.T) {
		_, err := seeded(t).CommitFiles(ctx, CommitOptions{StartRev: "main"})
		require.ErrorContains(t, err, "branch is required")
	})

	t.Run("from a revision that is not there", func(t *testing.T) {
		_, err := seeded(t).CommitFiles(ctx, CommitOptions{Branch: "main", StartRev: "ghost"})
		require.ErrorIs(t, err, ErrNotFound)
		require.ErrorContains(t, err, "start revision")
	})

	for _, tc := range []struct {
		desc   string
		action Action
	}{
		{desc: "a put over an existing file", action: Action{Kind: ActionPut, Path: "top.txt", Content: []byte("new")}},
		{desc: "a put over a directory", action: Action{Kind: ActionPut, Path: "a", Content: []byte("new")}},
		{desc: "a put below a file", action: Action{Kind: ActionPut, Path: "top.txt/below", Content: []byte("new")}},
		{desc: "a delete of a missing file", action: Action{Kind: ActionDelete, Path: "missing.txt"}},
		{desc: "a move of a missing file", action: Action{Kind: ActionMove, FromPath: "missing.txt", Path: "moved.txt"}},
		{desc: "a chmod of a missing file", action: Action{Kind: ActionChmod, Path: "missing.txt"}},
		{desc: "an unknown action", action: Action{Kind: ActionKind("bogus"), Path: "top.txt"}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := seeded(t).CommitFiles(ctx, CommitOptions{
				Branch:   "main",
				StartRev: "main",
				Actions:  []Action{tc.action},
			})
			require.Error(t, err)
		})
	}

	t.Run("a put that overwrites keeps the mode of the file", func(t *testing.T) {
		m := NewMemoryBackend()
		m.Put("main", "run.sh", []byte("echo one"))
		_, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "main",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionChmod, Path: "run.sh", Executable: true}},
		})
		require.NoError(t, err)

		id, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "main",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionPut, Path: "run.sh", Content: []byte("echo two"), Overwrite: true}},
		})
		require.NoError(t, err)
		require.Equal(t, modeExec, m.commits[id].files["run.sh"].mode)
	})

	t.Run("a chmod clears the executable bit", func(t *testing.T) {
		m := NewMemoryBackend()
		m.Put("main", "run.sh", []byte("echo"))
		_, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "main",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionChmod, Path: "run.sh", Executable: true}},
		})
		require.NoError(t, err)
		id, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "main",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionChmod, Path: "run.sh"}},
		})
		require.NoError(t, err)
		require.Equal(t, modeRegular, m.commits[id].files["run.sh"].mode)
	})

	t.Run("a move renames the file", func(t *testing.T) {
		m := NewMemoryBackend()
		m.Put("main", "a.txt", []byte("a"))
		id, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "main",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionMove, FromPath: "a.txt", Path: "b.txt"}},
		})
		require.NoError(t, err)
		require.Contains(t, m.commits[id].files, "b.txt")
		require.NotContains(t, m.commits[id].files, "a.txt")
	})
}

// TestMemoryBackendLand covers the outcomes of a land the memory backend can
// produce: a fast-forward, a branch that is created, a dry run, a conflict
// and its resolutions.
func TestMemoryBackendLand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("a branch that is not there", func(t *testing.T) {
		m := NewMemoryBackend()
		id := m.Put("main", "a.txt", []byte("a"))

		_, err := m.Land(ctx, LandOptions{Commit: id, Branch: "release"})
		require.ErrorIs(t, err, ErrNotFound)

		res, err := m.Land(ctx, LandOptions{Commit: id, Branch: "release", CreateBranch: true})
		require.NoError(t, err)
		require.Equal(t, id, res.Commit)
		require.Equal(t, id, m.branches["release"])
	})

	t.Run("a commit the branch already holds", func(t *testing.T) {
		m := NewMemoryBackend()
		id := m.Put("main", "a.txt", []byte("a"))

		res, err := m.Land(ctx, LandOptions{Commit: id, Branch: "main"})
		require.NoError(t, err)
		require.True(t, res.AlreadyLanded)
		require.Equal(t, id, res.Commit)
	})

	t.Run("a fast-forward", func(t *testing.T) {
		m := NewMemoryBackend()
		base := m.Put("main", "a.txt", []byte("a"))
		ahead, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "feature",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionPut, Path: "b.txt", Content: []byte("b")}},
		})
		require.NoError(t, err)
		require.Equal(t, base, m.commits[ahead].parent)

		res, err := m.Land(ctx, LandOptions{Commit: ahead, Branch: "main"})
		require.NoError(t, err)
		require.Equal(t, ahead, res.Commit)
		require.False(t, res.Rewritten)
		require.Equal(t, ahead, m.branches["main"])
	})

	t.Run("a dry run leaves the branch where it was", func(t *testing.T) {
		m := NewMemoryBackend()
		base := m.Put("main", "a.txt", []byte("a"))
		ahead, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "feature",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionPut, Path: "b.txt", Content: []byte("b")}},
		})
		require.NoError(t, err)

		res, err := m.Land(ctx, LandOptions{Commit: ahead, Branch: "main", DryRun: true})
		require.NoError(t, err)
		require.Equal(t, ahead, res.Commit)
		require.Equal(t, base, m.branches["main"])
	})

	t.Run("a land that is not fast-forward", func(t *testing.T) {
		m := NewMemoryBackend()
		m.Put("main", "a.txt", []byte("a"))
		theirs, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "feature",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionPut, Path: "a.txt", Content: []byte("theirs"), Overwrite: true}},
		})
		require.NoError(t, err)
		_, err = m.CommitFiles(ctx, CommitOptions{
			Branch:   "main",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionPut, Path: "a.txt", Content: []byte("mine"), Overwrite: true}},
		})
		require.NoError(t, err)

		_, err = m.Land(ctx, LandOptions{Commit: theirs, Branch: "main", Strategy: StrategyFFOnly})
		require.ErrorIs(t, err, ErrNotFastForward)

		_, err = m.Land(ctx, LandOptions{Commit: theirs, Branch: "main"})
		require.ErrorIs(t, err, ErrMergeConflict)
	})

	// diverged returns a backend whose main and feature branches both change
	// a.txt from the same base, with the commits of both branches named.
	diverged := func(t *testing.T) (*MemoryBackend, string, string) {
		t.Helper()

		m := NewMemoryBackend()
		m.Put("main", "a.txt", []byte("a"))
		theirs, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "feature",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionPut, Path: "a.txt", Content: []byte("theirs"), Overwrite: true}},
		})
		require.NoError(t, err)
		tip, err := m.CommitFiles(ctx, CommitOptions{
			Branch:   "main",
			StartRev: "main",
			Actions:  []Action{{Kind: ActionPut, Path: "a.txt", Content: []byte("mine"), Overwrite: true}},
		})
		require.NoError(t, err)
		return m, theirs, tip
	}

	t.Run("a resolution for another commit", func(t *testing.T) {
		m, theirs, tip := diverged(t)

		_, err := m.Land(ctx, LandOptions{Commit: theirs, Branch: "main", Resolution: &Resolution{Commit: tip, Tip: tip}})
		require.ErrorContains(t, err, "the resolution is for commit")
	})

	t.Run("a resolution against another tip", func(t *testing.T) {
		m, theirs, _ := diverged(t)

		_, err := m.Land(ctx, LandOptions{Commit: theirs, Branch: "main", Resolution: &Resolution{Commit: theirs, Tip: "another"}})
		require.ErrorIs(t, err, ErrMergeConflict)
	})

	t.Run("a resolution that leaves the conflict unresolved", func(t *testing.T) {
		m, theirs, tip := diverged(t)

		_, err := m.Land(ctx, LandOptions{Commit: theirs, Branch: "main", Resolution: &Resolution{Commit: theirs, Tip: tip}})
		require.ErrorIs(t, err, ErrMergeConflict)
	})

	t.Run("a resolution with a mode that is not octal", func(t *testing.T) {
		m, theirs, tip := diverged(t)

		_, err := m.Land(ctx, LandOptions{Commit: theirs, Branch: "main", Resolution: &Resolution{
			Commit: theirs, Tip: tip,
			Files: []ResolvedFile{{Path: "a.txt", Mode: "nope", Content: []byte("merged")}},
		}})
		require.Error(t, err)
	})

	t.Run("a resolution that names a file that does not conflict", func(t *testing.T) {
		m, theirs, tip := diverged(t)

		_, err := m.Land(ctx, LandOptions{Commit: theirs, Branch: "main", Resolution: &Resolution{
			Commit: theirs, Tip: tip,
			Files: []ResolvedFile{{Path: "a.txt", Mode: "100644", Content: []byte("merged")}, {Path: "other.txt", Mode: "100644", Content: []byte("x")}},
		}})
		require.ErrorIs(t, err, ErrMergeConflict)
	})

	t.Run("a resolution", func(t *testing.T) {
		m, theirs, tip := diverged(t)

		res, err := m.Land(ctx, LandOptions{Commit: theirs, Branch: "main", Resolution: &Resolution{
			Commit: theirs, Tip: tip,
			Files: []ResolvedFile{{Path: "a.txt", Mode: "100644", Content: []byte("merged")}},
		}})
		require.NoError(t, err)
		require.True(t, res.Rewritten)
		require.Equal(t, []byte("merged"), m.commits[res.Commit].files["a.txt"].content)
	})

	t.Run("a resolution that deletes the file", func(t *testing.T) {
		m, theirs, tip := diverged(t)

		res, err := m.Land(ctx, LandOptions{Commit: theirs, Branch: "main", Resolution: &Resolution{
			Commit: theirs, Tip: tip,
			Files: []ResolvedFile{{Path: "a.txt", Delete: true}},
		}})
		require.NoError(t, err)
		require.NotContains(t, m.commits[res.Commit].files, "a.txt")
	})
}

// TestMemoryBackendReportsThePathsItCannotRead covers the reads of the memory
// backend whose revision or path is not there.
func TestMemoryBackendReportsThePathsItCannotRead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	m := NewMemoryBackend()
	m.Put("main", "a/x.txt", []byte("one\ntwo\n"))

	_, err := m.ResolveRevision(ctx, "ghost")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = m.ListTree(ctx, "ghost", "", true)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = m.ListTree(ctx, "main", "a/x.txt", true)
	require.ErrorContains(t, err, "is a file, not a tree")

	_, err = m.Blame(ctx, "ghost", "a/x.txt")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = m.Blame(ctx, "main", "ghost.txt")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = m.PathHistory(ctx, "ghost", "a/x.txt", 0)
	require.ErrorIs(t, err, ErrNotFound)

	lines, err := m.Blame(ctx, "main", "a/x.txt")
	require.NoError(t, err)
	require.Len(t, lines, 2)

	history, err := m.PathHistory(ctx, "main", "a/x.txt", 0)
	require.NoError(t, err)
	require.NotEmpty(t, history)

	history, err = m.PathHistory(ctx, "main", "a/x.txt", 1)
	require.NoError(t, err)
	require.Len(t, history, 1)

	all, err := m.Changesets(ctx, 0, 0)
	require.NoError(t, err)
	require.NotEmpty(t, all)

	some, err := m.Changesets(ctx, 0, 1)
	require.NoError(t, err)
	require.Len(t, some, 1)

	none, err := m.Changesets(ctx, uint64(len(all)), 0)
	require.NoError(t, err)
	require.Empty(t, none)
}

// TestMemoryBackendUnknownStrategy covers the error of a land whose strategy
// the backend does not know, which is not a fast-forward.
func TestMemoryBackendUnknownStrategy(t *testing.T) {
	t.Parallel()

	m := NewMemoryBackend()
	m.Put("main", "a.txt", []byte("a"))

	_, err := m.Land(context.Background(), LandOptions{Commit: "ghost", Branch: "main"})
	require.True(t, errors.Is(err, ErrNotFound))
}
