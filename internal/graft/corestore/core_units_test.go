package corestore

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseMode covers the modes of the storage core and the name it refuses.
func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
	}{
		{in: "", want: ModeOff},
		{in: "off", want: ModeOff},
		{in: "shadow", want: ModeShadow},
		{in: "authoritative", want: ModeAuthoritative},
	} {
		got, err := ParseMode(tc.in)
		require.NoError(t, err, tc.in)
		require.Equal(t, tc.want, got, tc.in)
	}

	_, err := ParseMode("bogus")
	require.Error(t, err)
}

// TestTheCoreReportsItsMode covers the mode of a core, including the core
// that does not exist.
func TestTheCoreReportsItsMode(t *testing.T) {
	var none *Core
	require.False(t, none.Enabled())
	require.False(t, none.Authoritative())
	require.False(t, none.Shadow())

	off := &Core{Mode: ModeOff}
	require.False(t, off.Enabled())
	require.False(t, off.Authoritative())
	require.False(t, off.Shadow())

	empty := &Core{}
	require.False(t, empty.Enabled())

	shadow := &Core{Mode: ModeShadow}
	require.True(t, shadow.Enabled())
	require.False(t, shadow.Authoritative())
	require.True(t, shadow.Shadow())

	authoritative := &Core{Mode: ModeAuthoritative}
	require.True(t, authoritative.Enabled())
	require.True(t, authoritative.Authoritative())
	require.False(t, authoritative.Shadow())
}

// TestTheCoreClosesItsStores covers the release of the database and the
// bucket, and the failure of the release of the bucket.
func TestTheCoreClosesItsStores(t *testing.T) {
	var none *Core
	require.NoError(t, none.Close())

	require.NoError(t, (&Core{}).Close())

	meta := errMetaStore{err: errors.New("the database refused")}
	blobs := errBlobStore{err: errors.New("the bucket refused")}
	err := (&Core{Meta: meta, Blobs: blobs}).Close()
	require.Error(t, err)
	require.Equal(t, "the database refused", err.Error())

	err = (&Core{Blobs: blobs}).Close()
	require.Error(t, err)
	require.Equal(t, "the bucket refused", err.Error())
}

// errMetaStore is a metadata store whose release fails.
type errMetaStore struct {
	MetaStore
	err error
}

func (s errMetaStore) Close() error { return s.err }

// errBlobStore is a blob store whose release fails.
type errBlobStore struct {
	BlobStore
	err error
}

func (s errBlobStore) Close() error { return s.err }

// TestCheckRelativePath covers the relative path that names the data
// directory of Gitaly, which no request may name.
func TestCheckRelativePath(t *testing.T) {
	require.NoError(t, CheckRelativePath("shop.git"))
	require.NoError(t, CheckRelativePath("group/shop.git"))

	err := CheckRelativePath("+gitaly")
	require.ErrorIs(t, err, ErrReservedRelativePath)

	err = CheckRelativePath("+gitaly/keep")
	require.ErrorIs(t, err, ErrReservedRelativePath)
}

// TestParentRefNames covers the names that are a directory of a reference.
func TestParentRefNames(t *testing.T) {
	require.Equal(t, []string{"refs", "refs/heads"}, ParentRefNames("refs/heads/main"))
	require.Equal(t, []string{"refs"}, ParentRefNames("refs/heads"))
	require.Nil(t, ParentRefNames("main"))
}

// TestRefNameConflicts covers the pairs of references that git cannot hold
// together, and the order of the pairs.
func TestRefNameConflicts(t *testing.T) {
	require.Nil(t, RefNameConflicts(map[string]Target{"refs/heads/main": "abc"}))

	conflicts := RefNameConflicts(map[string]Target{
		"refs/heads/a":   "1",
		"refs/heads/a/b": "2",
		"refs/heads":     "3",
	})
	require.Len(t, conflicts, 3)
	require.Equal(t, "refs/heads", conflicts[0].Parent)
	require.Equal(t, "refs/heads/a", conflicts[0].Child)
	require.Equal(t, "refs/heads", conflicts[1].Parent)
	require.Equal(t, "refs/heads/a/b", conflicts[1].Child)
	require.Equal(t, "refs/heads/a", conflicts[2].Parent)
	require.Equal(t, "refs/heads/a/b", conflicts[2].Child)
}

// TestRefConflictError covers the message of a conflict of references.
func TestRefConflictError(t *testing.T) {
	require.Equal(t, "storage core: reference conflict", (&RefConflictError{}).Error())

	err := &RefConflictError{Conflicts: []RefConflict{{Name: "refs/heads/main", Actual: "a", Expected: "b"}}}
	require.Contains(t, err.Error(), "refs/heads/main")
	require.Contains(t, err.Error(), "1 conflicting references")
}

// TestTargetSymbolic covers the symbolic references of a target.
func TestTargetSymbolic(t *testing.T) {
	require.True(t, Target("").Absent())
	require.False(t, Target("abc").Absent())

	require.False(t, Target("abc").IsSymbolic())
	require.True(t, Target(SymbolicPrefix+"refs/heads/main").IsSymbolic())
	require.Equal(t, "refs/heads/main", Target(SymbolicPrefix+"refs/heads/main").SymbolicName())
	require.Empty(t, Target("abc").SymbolicName())
}

// TestSubscribeChangelog covers the subscription to the changes of a
// repository and its end.
func TestSubscribeChangelog(t *testing.T) {
	ch, cancel := SubscribeChangelog(4242)
	NotifyChangelog(4242)
	select {
	case <-ch:
	default:
		t.Fatal("the subscriber was not woken up")
	}

	// A second notification does not block, even when nobody reads.
	NotifyChangelog(4242)
	NotifyChangelog(4242)

	cancel()
	cancel()

	ch2, cancel2 := SubscribeChangelog(4242)
	defer cancel2()
	NotifyChangelog(4242)
	select {
	case <-ch2:
	default:
		t.Fatal("the second subscriber was not woken up")
	}
}
