package cachetier

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// brokenObjectStream is a GetObjects stream whose client is gone: every send
// fails, which is what a cache tier sees when the caller disconnects.
type brokenObjectStream struct {
	grpc.ServerStream
	ctx context.Context
	err error
}

func (s *brokenObjectStream) Context() context.Context                { return s.ctx }
func (s *brokenObjectStream) Send(*gitalypb.GetObjectsResponse) error { return s.err }
func (s *brokenObjectStream) SendMsg(any) error                       { return s.err }

// brokenTreeStream is a GetTree stream whose client is gone.
type brokenTreeStream struct {
	grpc.ServerStream
	ctx context.Context
	err error
}

func (s *brokenTreeStream) Context() context.Context             { return s.ctx }
func (s *brokenTreeStream) Send(*gitalypb.GetTreeResponse) error { return s.err }
func (s *brokenTreeStream) SendMsg(any) error                    { return s.err }

// TestTheHandlersReportAClientThatIsGone covers the sends of the cache
// handlers: an object, a missing object and a tree listing whose caller is no
// longer there are reported, so the tier does not count them as served.
func TestTheHandlersReportAClientThatIsGone(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	repo := testRepo("@hashed/gone-client.git")
	upstream := newFakeObjectService()
	oid := upstream.put(repo, gitalypb.ObjectType_BLOB, []byte("0123456789abcdefghijklmnopqrstuvwxyz"), false)
	missing := gitObjectID("blob", []byte("never stored"), false)
	treeOID := gitObjectID("tree", []byte("never stored either"), false)
	upstream.putTree(treeOID, []*gitalypb.GetTreeResponse_Entry{{
		Path: []byte("a.txt"),
		Oid:  gitObjectID("blob", []byte("a"), false),
		Type: gitalypb.ObjectType_BLOB,
		Mode: 0o100644,
	}})
	tier := newTier(t, upstream, Config{})

	gone := errors.New("the client is gone")

	t.Run("an object", func(t *testing.T) {
		err := tier.server.GetObjects(&gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{oid}},
			&brokenObjectStream{ctx: ctx, err: gone})
		require.ErrorIs(t, err, gone)
	})

	t.Run("a missing object", func(t *testing.T) {
		err := tier.server.GetObjects(&gitalypb.GetObjectsRequest{Repository: repo, Oids: []string{missing}},
			&brokenObjectStream{ctx: ctx, err: gone})
		require.ErrorIs(t, err, gone)
	})

	t.Run("a tree", func(t *testing.T) {
		err := tier.server.GetTree(&gitalypb.GetTreeRequest{Repository: repo, TreeOid: treeOID},
			&brokenTreeStream{ctx: ctx, err: gone})
		require.ErrorIs(t, err, gone)
	})
}
