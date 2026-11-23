package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestConflictsWriterCreatesEachFileOnce covers the writer of a conflicts
// directory: a target is created once, an existing one names the conflicting
// path that took it, and a parent that is a file is reported with the path.
func TestConflictsWriterCreatesEachFileOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	w := conflictsWriter{written: map[string]string{}}

	target := filepath.Join(dir, "files", "a.txt")
	require.NoError(t, w.create(target, "a.txt", []byte("one")))
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "one", string(content))

	// The same target from another conflicting path: the writer knows which one
	// took it, which is what an operator needs to resolve the conflict elsewhere.
	err = w.create(target, "A.txt", nil)
	require.ErrorContains(t, err, `conflicting paths "a.txt" and "A.txt"`)

	// A file the command did not write has no conflicting path to name.
	other := filepath.Join(dir, "files", "b.txt")
	require.NoError(t, os.WriteFile(other, nil, 0o644))
	err = w.create(other, "b.txt", nil)
	require.ErrorContains(t, err, "another conflicting path")

	// A parent that is a file cannot hold the directory of the conflict.
	blocked := filepath.Join(dir, "files", "blocked")
	require.NoError(t, os.WriteFile(blocked, nil, 0o644))
	err = w.create(filepath.Join(blocked, "x.txt"), "x.txt", nil)
	require.ErrorContains(t, err, "x.txt")
}

// TestWriteProposal covers the proposal of a conflict: a deletion has no file,
// a proposal the server cannot return is reported, and an empty blob is written
// as an empty file.
func TestWriteProposalReportsWhatItCannotWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var out bytes.Buffer
	created := map[string]string{}
	create := func(target, p string, data []byte) error {
		created[p] = string(data)
		return nil
	}

	// A deletion only says so.
	require.NoError(t, writeProposal(&out, dir, &gitalypb.LandConflictFile{
		Path:     []byte("a.txt"),
		Proposal: &gitalypb.LandConflictProposal{Resolver: "ai", Action: "delete"},
	}, nil, nil, create))
	require.Contains(t, out.String(), "proposes to delete it")

	// A proposal whose blob the server does not return fails nothing else.
	out.Reset()
	require.NoError(t, writeProposal(&out, dir, &gitalypb.LandConflictFile{
		Path:     []byte("a.txt"),
		Proposal: &gitalypb.LandConflictProposal{Resolver: "ai", Oid: "ab12"},
	}, map[string][]byte{}, map[string]string{"ab12": "the object is gone"}, create))
	require.Contains(t, out.String(), "the proposal ab12 of ai cannot be read: the object is gone")

	// An empty blob is a proposal that empties the file.
	out.Reset()
	require.NoError(t, writeProposal(&out, dir, &gitalypb.LandConflictFile{
		Path:     []byte("a.txt"),
		Proposal: &gitalypb.LandConflictProposal{Resolver: "ai", Oid: "cd34"},
	}, map[string][]byte{"cd34": nil}, nil, create))
	require.Equal(t, "", created["a.txt"])
	require.Contains(t, out.String(), "proposes proposals/a.txt")

	// A file that cannot be written fails the command.
	wantErr := os.ErrPermission
	require.ErrorIs(t, writeProposal(&out, dir, &gitalypb.LandConflictFile{
		Path:     []byte("a.txt"),
		Proposal: &gitalypb.LandConflictProposal{Resolver: "ai", Oid: "cd34"},
	}, map[string][]byte{"cd34": []byte("x")}, nil, func(string, string, []byte) error { return wantErr }), wantErr)
}

// TestWriteConflictsRefusesADirectoryThatHoldsOne covers the guard of the
// conflicts directory: it is refused when it holds files already, so that the
// conflicts of two lands are never mixed.
func TestWriteConflictsRefusesADirectoryThatHoldsOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, conflictsFilesDir), 0o755))

	var out bytes.Buffer
	err := writeConflicts(t.Context(), nil, &gitalypb.LandConflict{}, dir, &out)
	require.ErrorContains(t, err, "already holds a conflicts directory")
}
