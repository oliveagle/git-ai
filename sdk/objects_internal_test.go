package sdk

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestTheReceiverRefusesTheObjectsTheRequestDidNotAskFor covers the stream
// that answers with an object out of order, or with one the caller never
// asked for: a reassembly that trusted it would hand a caller content under
// the wrong object ID.
func TestTheReceiverRefusesTheObjectsTheRequestDidNotAskFor(t *testing.T) {
	rc := &objectReceiver{expected: []string{"a", "b"}}

	err := rc.handle(&gitalypb.GetObjectsResponse{Oid: "b", Type: gitalypb.ObjectType_BLOB})
	require.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(t, err, `unexpected object "b"`)
}

// TestTheReceiverRefusesAnObjectThatChangesUnderIt covers the stream that
// starts a second object before the first one is complete: the content of the
// first is lost, so the receiver refuses instead of stitching the two.
func TestTheReceiverRefusesAnObjectThatChangesUnderIt(t *testing.T) {
	rc := &objectReceiver{expected: []string{"a", "b"}}

	require.NoError(t, rc.handle(&gitalypb.GetObjectsResponse{
		Oid: "a", Type: gitalypb.ObjectType_BLOB, Size: 8, Data: []byte("half"),
	}))
	err := rc.handle(&gitalypb.GetObjectsResponse{
		Oid: "b", Type: gitalypb.ObjectType_BLOB, Size: 1, Data: []byte("b"),
	})
	require.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(t, err, `object "a" is incomplete`)
}

// TestTheReceiverRefusesAChunkAtTheWrongOffset covers the chunk that does not
// continue where the previous one stopped.
func TestTheReceiverRefusesAChunkAtTheWrongOffset(t *testing.T) {
	rc := &objectReceiver{expected: []string{"a"}}

	require.NoError(t, rc.handle(&gitalypb.GetObjectsResponse{
		Oid: "a", Type: gitalypb.ObjectType_BLOB, Size: 4, Data: []byte("ab"),
	}))
	err := rc.handle(&gitalypb.GetObjectsResponse{
		Oid: "a", Offset: 0, Data: []byte("cd"),
	})
	require.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(t, err, "chunk at offset 0, expected 2")
}

// TestTheReceiverCompletesAMissingObject covers the object the server does not
// hold: it is complete as soon as it is reported missing, and the flush moves
// it to the result.
func TestTheReceiverCompletesAMissingObject(t *testing.T) {
	rc := &objectReceiver{expected: []string{"a"}}
	require.NoError(t, rc.handle(&gitalypb.GetObjectsResponse{Oid: "a", Missing: true}))
	rc.flush()

	require.True(t, rc.complete())
	require.Len(t, rc.done, 1)
	require.True(t, rc.done[0].Missing)
}

// TestTheReceiverKeepsAnIncompleteObject covers the flush of an object that is
// still in progress: it stays partial, so a resumed request continues it.
func TestTheReceiverKeepsAnIncompleteObject(t *testing.T) {
	rc := &objectReceiver{expected: []string{"a"}}
	require.NoError(t, rc.handle(&gitalypb.GetObjectsResponse{
		Oid: "a", Type: gitalypb.ObjectType_BLOB, Size: 4, Data: []byte("ab"),
	}))
	rc.flush()

	require.False(t, rc.complete())
	require.NotNil(t, rc.partial)
	require.Empty(t, rc.done)
}

// TestObjectLimitReadsTheMaximumOfTheServer covers the refusal of a batch the
// server holds too many IDs for: the named maximum, the halved batch when the
// server names none, and the errors that are not that refusal.
func TestObjectLimitReadsTheMaximumOfTheServer(t *testing.T) {
	limit, ok := objectLimit(status.Error(codes.InvalidArgument, "too many object IDs: 100, maximum is 25"), 100)
	require.True(t, ok)
	require.Equal(t, 25, limit)

	limit, ok = objectLimit(status.Error(codes.InvalidArgument, "too many object IDs: 100"), 100)
	require.True(t, ok)
	require.Equal(t, 50, limit)

	// A maximum the server names that does not explain the batch is ignored,
	// and the batch is halved instead.
	limit, ok = objectLimit(status.Error(codes.InvalidArgument, "too many object IDs: 100, maximum is 200"), 100)
	require.True(t, ok)
	require.Equal(t, 50, limit)

	_, ok = objectLimit(status.Error(codes.Internal, "too many object IDs: 100"), 100)
	require.False(t, ok, "only InvalidArgument is the refusal")

	_, ok = objectLimit(status.Error(codes.InvalidArgument, "another refusal"), 100)
	require.False(t, ok)

	_, ok = objectLimit(status.Error(codes.InvalidArgument, "too many object IDs"), 1)
	require.False(t, ok, "a single ID is never refused this way")
}

// TestUniqueOIDsRefusesAnEmptyIdentifier covers the batch: duplicates are read
// once, and an empty identifier is a caller's mistake.
func TestUniqueOIDsRefusesAnEmptyIdentifier(t *testing.T) {
	got, err := uniqueOIDs([]string{"a", "b", "a"})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, got)

	_, err = uniqueOIDs([]string{"a", ""})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
