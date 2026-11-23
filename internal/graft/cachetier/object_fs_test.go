package cachetier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestParseTypeName covers every Git name the cache stores, and the name it
// does not know.
func TestParseTypeName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		typ  gitalypb.ObjectType
		ok   bool
	}{
		{name: "commit", typ: gitalypb.ObjectType_COMMIT, ok: true},
		{name: "blob", typ: gitalypb.ObjectType_BLOB, ok: true},
		{name: "tree", typ: gitalypb.ObjectType_TREE, ok: true},
		{name: "tag", typ: gitalypb.ObjectType_TAG, ok: true},
		{name: "blame", typ: gitalypb.ObjectType_UNKNOWN, ok: false},
		{name: "", typ: gitalypb.ObjectType_UNKNOWN, ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ, ok := parseTypeName(tc.name)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.typ, typ)
		})
	}
}

// TestVerifyObjectRefusesAnObjectIDOfAnotherLength covers the object ID whose
// length is neither SHA-1 nor SHA-256.
func TestVerifyObjectRefusesAnObjectIDOfAnotherLength(t *testing.T) {
	t.Parallel()

	err := verifyObject("abc", &object{typ: gitalypb.ObjectType_BLOB, data: []byte("x")})
	require.ErrorIs(t, err, errVerification)
	require.Contains(t, err.Error(), "invalid object ID length 3")
}

// TestNewDiskStoreReportsAnUnusableRoot covers the root the store cannot
// create and the root it cannot scan.
func TestNewDiskStoreReportsAnUnusableRoot(t *testing.T) {
	t.Parallel()

	t.Run("a root below a file", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "not-a-directory")
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

		_, err := newDiskStore(filepath.Join(file, "objects"), DefaultDiskMaxBytes)
		require.ErrorContains(t, err, "create disk store")
	})

	t.Run("a root with an unreadable directory", func(t *testing.T) {
		dir := t.TempDir()
		locked := filepath.Join(dir, "locked")
		require.NoError(t, os.Mkdir(locked, 0o755))
		require.NoError(t, os.Chmod(locked, 0o000))
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

		_, err := newDiskStore(dir, DefaultDiskMaxBytes)
		require.ErrorContains(t, err, "scan disk store")
	})
}

// TestDiskStoreReadDiscardsAnUnreadableEntry covers the damaged entries the
// store reads as a miss instead of an error.
func TestDiskStoreReadDiscardsAnUnreadableEntry(t *testing.T) {
	t.Parallel()

	data := []byte("blob")
	oid := gitObjectID("blob", data, false)
	key := objectKey{storage: "default", relativePath: "repo.git", oid: oid}

	store, err := newDiskStore(t.TempDir(), DefaultDiskMaxBytes)
	require.NoError(t, err)
	path := store.path(key)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))

	for _, tc := range []struct {
		desc    string
		content string
	}{
		{desc: "without a size", content: "blob\nblob"},
		{desc: "with an unknown type", content: "blame 4\nblob"},
		{desc: "with a size that is not a number", content: "blob nope\nblob"},
		{desc: "with a size that is negative", content: "blob -1\nblob"},
		{desc: "with more bytes than the size says", content: "blob 4\nblob!"},
		{desc: "with fewer bytes than the size says", content: "blob 9\nblob"},
		{desc: "whose content does not hash to the name", content: "blob 4\nblah"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o644))

			got, err := store.read(key, noReadLimit)
			require.NoError(t, err)
			require.Nil(t, got)
			require.NoFileExists(t, path)
		})
	}
}

// TestDiskStoreWriteReportsTheFailuresOfTheFileSystem covers the writes the
// file system refuses and the budget the store cannot enforce.
func TestDiskStoreWriteReportsTheFailuresOfTheFileSystem(t *testing.T) {
	t.Parallel()

	data := []byte("blob")
	oid := gitObjectID("blob", data, false)
	key := objectKey{storage: "default", relativePath: "repo.git", oid: oid}

	t.Run("an object type it cannot store", func(t *testing.T) {
		store, err := newDiskStore(t.TempDir(), DefaultDiskMaxBytes)
		require.NoError(t, err)

		err = store.write(key, &object{typ: gitalypb.ObjectType_UNKNOWN, data: data})
		require.ErrorContains(t, err, "unsupported object type")
	})

	t.Run("a directory it cannot create", func(t *testing.T) {
		root := t.TempDir()
		store, err := newDiskStore(root, DefaultDiskMaxBytes)
		require.NoError(t, err)

		rel, err := filepath.Rel(root, store.path(key))
		require.NoError(t, err)
		first, _, _ := strings.Cut(rel, string(filepath.Separator))
		require.NoError(t, os.WriteFile(filepath.Join(root, first), []byte("x"), 0o644))

		err = store.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: data})
		require.ErrorContains(t, err, "create object directory")
	})

	t.Run("a temporary file it cannot create", func(t *testing.T) {
		store, err := newDiskStore(t.TempDir(), DefaultDiskMaxBytes)
		require.NoError(t, err)

		dir := filepath.Dir(store.path(key))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.Chmod(dir, 0o555))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

		err = store.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: data})
		require.ErrorContains(t, err, "create temporary object")
	})

	t.Run("a path it cannot publish over", func(t *testing.T) {
		store, err := newDiskStore(t.TempDir(), DefaultDiskMaxBytes)
		require.NoError(t, err)

		path := store.path(key)
		require.NoError(t, os.MkdirAll(path, 0o755))

		err = store.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: data})
		require.ErrorContains(t, err, "publish object")
	})

	t.Run("a budget it cannot enforce", func(t *testing.T) {
		root := t.TempDir()
		store, err := newDiskStore(root, 1)
		require.NoError(t, err)

		locked := filepath.Join(root, "locked")
		require.NoError(t, os.Mkdir(locked, 0o755))
		require.NoError(t, os.Chmod(locked, 0o000))
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

		err = store.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: data})
		require.ErrorContains(t, err, "enforce disk budget")
	})
}

// TestDiskStoreReadsAnObjectAboveTheLimit covers the limit of a read that
// refuses the object before loading it.
func TestDiskStoreReadsAnObjectAboveTheLimit(t *testing.T) {
	t.Parallel()

	data := []byte("an object that is longer than the limit")
	oid := gitObjectID("blob", data, false)
	key := objectKey{storage: "default", relativePath: "repo.git", oid: oid}

	store, err := newDiskStore(t.TempDir(), DefaultDiskMaxBytes)
	require.NoError(t, err)
	require.NoError(t, store.write(key, &object{typ: gitalypb.ObjectType_BLOB, data: data}))

	got, err := store.read(key, 3)
	require.ErrorIs(t, err, errObjectOverLimit)
	require.Nil(t, got)

	// The file is not discarded: the limit is the caller's, not the entry's.
	require.FileExists(t, store.path(key))
}
