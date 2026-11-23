package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// blobBackend serves the blobs a conflicts package names, and can fail the
// first call, which is the all-at-once read the package tries first.
type blobBackend struct {
	workspace.Backend
	blobs    map[string][]byte
	err      error
	calls    int
	oneByOne bool
}

func (b *blobBackend) GetObjects(ctx context.Context, oids []string) (map[string][]byte, error) {
	b.calls++
	if b.err != nil && !(b.oneByOne && b.calls > 1) {
		return nil, b.err
	}
	out := map[string][]byte{}
	for _, oid := range oids {
		if content, ok := b.blobs[oid]; ok {
			out[oid] = content
		}
	}
	return out, nil
}

// conflictWorkspace returns a workspace whose backend serves the blobs of the
// test.
func conflictWorkspace(t *testing.T, b *blobBackend) *workspace.Workspace {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "ws")
	mem := workspace.NewMemoryBackend()
	mem.Put("main", "a/x.txt", []byte("one"))
	_, err := workspace.Clone(context.Background(), dir, mem, workspace.CloneOptions{Branch: "main"})
	require.NoError(t, err)

	ws, err := workspace.Open(dir, b)
	require.NoError(t, err)
	return ws
}

// TestWriteConflictsRefusesAPathItCannotWrite covers the conflicting path that
// cannot be written into the conflicts directory.
func TestWriteConflictsRefusesAPathItCannotWrite(t *testing.T) {
	t.Parallel()

	ws := conflictWorkspace(t, &blobBackend{})
	err := writeConflicts(context.Background(), ws, &gitalypb.LandConflict{
		Files: []*gitalypb.LandConflictFile{{Path: []byte("../escape")}},
	}, t.TempDir(), nil)
	require.ErrorContains(t, err, "cannot be written")
}

// TestWriteConflictsReportsTheSidesItCannotRead covers the side and the
// proposal whose blob is missing or empty: the package says so and still
// writes the file.
func TestWriteConflictsReportsTheSidesItCannotRead(t *testing.T) {
	t.Parallel()

	ws := conflictWorkspace(t, &blobBackend{blobs: map[string][]byte{"empty": nil}})
	dir := t.TempDir()
	var out bytes.Buffer

	err := writeConflicts(context.Background(), ws, &gitalypb.LandConflict{
		Files: []*gitalypb.LandConflictFile{{
			Path:   []byte("a.txt"),
			Base:   &gitalypb.LandConflictSide{Oid: "missing"},
			Ours:   &gitalypb.LandConflictSide{Oid: "empty"},
			Theirs: &gitalypb.LandConflictSide{Oid: ""},
			Proposal: &gitalypb.LandConflictProposal{
				Oid: "gone",
			},
		}},
	}, dir, &out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "cannot be read")
	require.Contains(t, out.String(), "the proposal gone")
	require.FileExists(t, filepath.Join(dir, conflictsFilesDir, "a.txt"))
	require.FileExists(t, filepath.Join(dir, conflictsPackage))
}

// TestWriteConflictsWritesTheProposals covers the proposal that deletes the
// path, and the one that writes a resolution next to the sides.
func TestWriteConflictsWritesTheProposals(t *testing.T) {
	t.Parallel()

	ws := conflictWorkspace(t, &blobBackend{blobs: map[string][]byte{
		"base": []byte("base\n"),
		"ours": []byte("ours\n"),
		"prop": []byte("resolved\n"),
	}})
	dir := t.TempDir()
	var out bytes.Buffer

	err := writeConflicts(context.Background(), ws, &gitalypb.LandConflict{
		Files: []*gitalypb.LandConflictFile{
			{
				Path: []byte("a.txt"),
				Base: &gitalypb.LandConflictSide{Oid: "base"},
				Ours: &gitalypb.LandConflictSide{Oid: "ours"},
				Proposal: &gitalypb.LandConflictProposal{
					Action:   "delete",
					Resolver: "agent",
				},
			},
			{
				Path: []byte("b.txt"),
				Base: &gitalypb.LandConflictSide{Oid: "base"},
				Ours: &gitalypb.LandConflictSide{Oid: "ours"},
				Proposal: &gitalypb.LandConflictProposal{
					Oid:      "prop",
					Resolver: "agent",
				},
			},
		},
	}, dir, &out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "proposes to delete it")
	require.Contains(t, out.String(), "proposes proposals/b.txt")
	require.FileExists(t, filepath.Join(dir, conflictsProposalsDir, "b.txt"))
}

// TestWriteConflictsRefusesAProposalItCannotWrite covers the proposal whose
// file the file system will not take.
func TestWriteConflictsRefusesAProposalItCannotWrite(t *testing.T) {
	t.Parallel()

	ws := conflictWorkspace(t, &blobBackend{blobs: map[string][]byte{"prop": []byte("x")}})
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, conflictsProposalsDir, "a.txt"), 0o755))

	err := writeConflicts(context.Background(), ws, &gitalypb.LandConflict{
		Files: []*gitalypb.LandConflictFile{{
			Path:     []byte("a.txt"),
			Proposal: &gitalypb.LandConflictProposal{Oid: "prop"},
		}},
	}, dir, nil)
	require.Error(t, err)
}

// TestWriteConflictsFetchesTheSidesOneByOne covers the all-at-once read that
// the server cannot answer: the package reads each blob on its own, and keeps
// the ones it gets.
func TestWriteConflictsFetchesTheSidesOneByOne(t *testing.T) {
	t.Parallel()

	ws := conflictWorkspace(t, &blobBackend{
		blobs:    map[string][]byte{"base": []byte("base\n")},
		err:      context.DeadlineExceeded,
		oneByOne: true,
	})
	dir := t.TempDir()
	var out bytes.Buffer

	err := writeConflicts(context.Background(), ws, &gitalypb.LandConflict{
		Files: []*gitalypb.LandConflictFile{{
			Path: []byte("a.txt"),
			Base: &gitalypb.LandConflictSide{Oid: "base"},
			Ours: &gitalypb.LandConflictSide{Oid: "missing"},
		}},
	}, dir, &out)
	require.NoError(t, err)
	require.Contains(t, out.String(), "cannot be read")
	require.FileExists(t, filepath.Join(dir, conflictsSidesDir, conflictSides[0], "a.txt"))
}

// TestWriteConflictsRefusesAPackageItCannotEncode covers the package that
// protojson refuses.
func TestWriteConflictsRefusesAPackageItCannotEncode(t *testing.T) {
	t.Parallel()

	ws := conflictWorkspace(t, &blobBackend{})
	err := writeConflicts(context.Background(), ws, &gitalypb.LandConflict{
		Tip: string([]byte{0xff}),
	}, t.TempDir(), nil)
	require.Error(t, err)
}
