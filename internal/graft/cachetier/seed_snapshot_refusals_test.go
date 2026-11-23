package cachetier

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// badSnapshotUpstream is an upstream whose tree and object streams answer what
// the test decides: a snapshot must refuse what a real upstream never sends
// before a byte of it reaches the cache.
type badSnapshotUpstream struct {
	*fakeObjectService
	tree    func(stream gitalypb.ObjectService_GetTreeServer) error
	objects func(stream gitalypb.ObjectService_GetObjectsServer) error
}

func (u *badSnapshotUpstream) GetTree(_ *gitalypb.GetTreeRequest, stream gitalypb.ObjectService_GetTreeServer) error {
	return u.tree(stream)
}

func (u *badSnapshotUpstream) GetObjects(_ *gitalypb.GetObjectsRequest, stream gitalypb.ObjectService_GetObjectsServer) error {
	return u.objects(stream)
}

// TestWriteSnapshotRefusesWhatAnUpstreamMustNotSend covers the validation of a
// snapshot: every answer that a well-formed upstream never gives stops the
// snapshot with the reason, instead of writing a seed file the cache would
// replay as if it were sound.
func TestWriteSnapshotRefusesWhatAnUpstreamMustNotSend(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/refusals.git")

	upstream := newFakeObjectService()
	root := upstream.put(repo, gitalypb.ObjectType_TREE, []byte("root listing"), false)
	blob := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("alpha\n"), false)

	// treeOf answers a tree with the root and the entries of the case.
	treeOf := func(rootOID string, entries ...*gitalypb.GetTreeResponse_Entry) func(stream gitalypb.ObjectService_GetTreeServer) error {
		return func(stream gitalypb.ObjectService_GetTreeServer) error {
			return stream.Send(&gitalypb.GetTreeResponse{TreeOid: rootOID, Entries: entries})
		}
	}
	// objectsOf sends the messages of the case and ends the stream.
	objectsOf := func(msgs ...*gitalypb.GetObjectsResponse) func(stream gitalypb.ObjectService_GetObjectsServer) error {
		return func(stream gitalypb.ObjectService_GetObjectsServer) error {
			for _, msg := range msgs {
				if err := stream.Send(msg); err != nil {
					return err
				}
			}
			return nil
		}
	}

	for _, tc := range []struct {
		name    string
		upstream *badSnapshotUpstream
		message string
	}{
		{
			name: "a tree ID that is not one",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf("not-an-object-id"),
			},
			message: "invalid tree ID",
		},
		{
			name: "an entry ID that is not one",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree: treeOf(root, &gitalypb.GetTreeResponse_Entry{
					Path: []byte("a.txt"), Oid: "zz", Type: gitalypb.ObjectType_BLOB,
				}),
			},
			message: "invalid object ID",
		},
		{
			name: "an entry of a type a repository does not hold",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree: treeOf(root, &gitalypb.GetTreeResponse_Entry{
					Path: []byte("a.txt"), Oid: blob, Type: gitalypb.ObjectType(99),
				}),
			},
			message: "unexpected type",
		},
		{
			name: "a stream without a root tree",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf(""),
			},
			message: "did not report the root tree",
		},
		{
			name: "a tree stream that fails",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree: func(stream gitalypb.ObjectService_GetTreeServer) error {
					return errors.New("the upstream went away")
				},
			},
			message: "list tree of",
		},
		{
			name: "an object that does not start at its offset",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf(root),
				objects: objectsOf(&gitalypb.GetObjectsResponse{
					Oid: root, Offset: 3, Type: gitalypb.ObjectType_TREE, Size: 5, Data: []byte("root"),
				}),
			},
			message: "starts at offset 3",
		},
		{
			name: "an object of a type the cache does not hold",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf(root),
				objects: objectsOf(&gitalypb.GetObjectsResponse{
					Oid: root, Type: gitalypb.ObjectType(99), Size: 4, Data: []byte("root"),
				}),
			},
			message: "unsupported type",
		},
		{
			name: "an object with a negative size",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf(root),
				objects: objectsOf(&gitalypb.GetObjectsResponse{
					Oid: root, Type: gitalypb.ObjectType_TREE, Size: -1,
				}),
			},
			message: "negative size",
		},
		{
			name: "a chunk at the wrong offset",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf(root),
				objects: objectsOf(
					&gitalypb.GetObjectsResponse{Oid: root, Offset: 0, Type: gitalypb.ObjectType_TREE, Size: 8, Data: []byte("root")},
					&gitalypb.GetObjectsResponse{Oid: root, Offset: 5, Data: []byte("list")},
				),
			},
			message: "chunk at offset 5",
		},
		{
			name: "more data than the declared size",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf(root),
				objects: objectsOf(&gitalypb.GetObjectsResponse{
					Oid: root, Offset: 0, Type: gitalypb.ObjectType_TREE, Size: 2, Data: []byte("root"),
				}),
			},
			message: "more data than its declared size",
		},
		{
			name: "an object shorter than its declared size",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf(root),
				objects: objectsOf(&gitalypb.GetObjectsResponse{
					Oid: root, Offset: 0, Type: gitalypb.ObjectType_TREE, Size: 8, Data: []byte("root"),
				}),
			},
			message: "got 4 of 8 bytes",
		},
		{
			name: "an object stream that fails",
			upstream: &badSnapshotUpstream{
				fakeObjectService: upstream,
				tree:              treeOf(root),
				objects: func(stream gitalypb.ObjectService_GetObjectsServer) error {
					return errors.New("the upstream went away")
				},
			},
			message: "read objects",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			addr := startGRPC(t, func(s *grpc.Server) {
				gitalypb.RegisterObjectServiceServer(s, tc.upstream)
			})

			var seed bytes.Buffer
			_, err := WriteSnapshot(ctx, dialUpstream(t, addr), "", repo, "main", &seed)
			require.ErrorContains(t, err, tc.message)
		})
	}
}

// TestWriteSnapshotReportsAMissingObject covers the object the upstream answers
// as missing: it is counted and left out of the seed, not written as an empty
// one.
func TestWriteSnapshotReportsAMissingObject(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/missing.git")

	upstream := newFakeObjectService()
	root := upstream.put(repo, gitalypb.ObjectType_TREE, []byte("root listing"), false)

	addr := startGRPC(t, func(s *grpc.Server) {
		gitalypb.RegisterObjectServiceServer(s, &badSnapshotUpstream{
			fakeObjectService: upstream,
			tree: func(stream gitalypb.ObjectService_GetTreeServer) error {
				return stream.Send(&gitalypb.GetTreeResponse{TreeOid: root})
			},
			objects: func(stream gitalypb.ObjectService_GetObjectsServer) error {
				return stream.Send(&gitalypb.GetObjectsResponse{Oid: root, Missing: true})
			},
		})
	})

	var seed bytes.Buffer
	report, err := WriteSnapshot(ctx, dialUpstream(t, addr), "", repo, "main", &seed)
	require.NoError(t, err)
	require.EqualValues(t, 1, report.Missing)
	require.Zero(t, report.Objects)
}

// failingWriter is a writer the snapshot cannot write a byte to.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestWriteSnapshotReportsAWriterThatFails covers the file of the snapshot that
// cannot be written: the snapshot fails instead of reporting a seed it did not
// write.
func TestWriteSnapshotReportsAWriterThatFails(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	fx := newSnapshotFixture(t)

	_, err := WriteSnapshot(ctx, dialUpstream(t, fx.addr), "", fx.repo, "main", failingWriter{})
	require.ErrorContains(t, err, "write seed header")
}
