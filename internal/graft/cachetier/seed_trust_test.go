package cachetier

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// seedWithFrames builds a compressed seed that holds the given raw frames after
// the header, so that one record can be damaged in exactly one way.
func seedWithFrames(t *testing.T, frames ...[]byte) []byte {
	t.Helper()

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	_, err := gz.Write([]byte(seedHeader))
	require.NoError(t, err)
	for _, frame := range frames {
		_, err := gz.Write(frame)
		require.NoError(t, err)
	}
	require.NoError(t, gz.Close())
	return out.Bytes()
}

// framed wraps a record body in its length prefix.
func framed(body []byte) []byte {
	return append(binary.AppendUvarint(nil, uint64(len(body))), body...)
}

// TestSeedRefusesTheRecordsItCannotTrust covers the validation of one record:
// the type the cache does not hold, the object ID that is not one, the record
// that names no repository, the body that does not fill its frame, a field
// longer than any seed names, and the frame that claims to be longer than the
// stream can hold.
func TestSeedRefusesTheRecordsItCannotTrust(t *testing.T) {
	t.Parallel()

	repo := testRepo("@hashed/trust.git")
	oid := gitObjectID("blob", []byte("data"), false)
	header := func(storage, path, oid string, typ gitalypb.ObjectType, size uint64) []byte {
		return rawSeedRecordHeader(storage, path, oid, typ, size)
	}

	for _, tc := range []struct {
		name   string
		frames [][]byte
	}{
		{
			name:   "a type the cache does not hold",
			frames: [][]byte{framed(header(repo.GetStorageName(), repo.GetRelativePath(), oid, gitalypb.ObjectType(99), 4))},
		},
		{
			name:   "an object ID that is not one",
			frames: [][]byte{framed(header(repo.GetStorageName(), repo.GetRelativePath(), "zz", gitalypb.ObjectType_BLOB, 0))},
		},
		{
			name:   "a record that names no repository",
			frames: [][]byte{framed(header("", repo.GetRelativePath(), oid, gitalypb.ObjectType_BLOB, 0))},
		},
		{
			name:   "a body that does not fill its frame",
			frames: [][]byte{framed(header(repo.GetStorageName(), repo.GetRelativePath(), oid, gitalypb.ObjectType_BLOB, 9))},
		},
		{
			name: "a field longer than any seed names",
			frames: [][]byte{framed(append(
				binary.AppendUvarint(nil, maxSeedField+1),
				make([]byte, maxSeedField+1)...,
			))},
		},
		{
			name:   "a frame longer than the stream can hold",
			frames: [][]byte{binary.AppendUvarint(nil, 1<<63)},
		},
		{
			name: "a frame that ends inside the header",
			frames: [][]byte{append(
				binary.AppendUvarint(nil, 40),
				binary.AppendUvarint(nil, 30)..., // a storage field of 30 bytes the frame does not hold
			)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tier := newTier(t, newFakeObjectService(), Config{MaxBytes: 1 << 20})
			report, err := tier.server.Seed(bytes.NewReader(seedWithFrames(t, tc.frames...)))
			require.NoError(t, err, "a damaged record is counted, never an error")
			require.GreaterOrEqual(t, report.Corrupt, int64(1))
			require.Zero(t, report.Objects)
		})
	}
}

// TestSeedWriterRefusesARecordItCannotEncode covers the writer: an object ID
// that is not one and a type the cache does not hold stop the write before a
// byte of the record is framed.
func TestSeedWriterRefusesARecordItCannotEncode(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	w, err := newSeedWriter(&out)
	require.NoError(t, err)

	err = w.write(seedRecord{oid: "zz", typ: gitalypb.ObjectType_BLOB})
	require.ErrorContains(t, err, "invalid object ID")

	err = w.write(seedRecord{oid: gitObjectID("blob", []byte("x"), false), typ: gitalypb.ObjectType(99)})
	require.ErrorContains(t, err, "unsupported type")
}

// TestNewObjectHasherRefusesWhatItCannotHash covers the hasher: an object of a
// type the cache does not hold, and an object ID whose length names no
// algorithm.
func TestNewObjectHasherRefusesWhatItCannotHash(t *testing.T) {
	t.Parallel()

	_, ok := newObjectHasher(gitObjectID("blob", []byte("x"), false), gitalypb.ObjectType(99), 1)
	require.False(t, ok)

	_, ok = newObjectHasher("short", gitalypb.ObjectType_BLOB, 1)
	require.False(t, ok)

	sha1, ok := newObjectHasher(gitObjectID("blob", []byte("x"), false), gitalypb.ObjectType_BLOB, 1)
	require.True(t, ok)
	require.Len(t, sha1.Sum(nil), 20)

	sha256, ok := newObjectHasher(gitObjectID("blob", []byte("x"), true), gitalypb.ObjectType_BLOB, 1)
	require.True(t, ok)
	require.Len(t, sha256.Sum(nil), 32)
}
