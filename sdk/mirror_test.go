package sdk_test

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// fakeRemote records a mirror request and answers with one divergent branch.
type fakeRemote struct {
	gitalypb.UnimplementedRemoteServiceServer

	requests []*gitalypb.UpdateRemoteMirrorRequest
}

func (f *fakeRemote) UpdateRemoteMirror(stream gitalypb.RemoteService_UpdateRemoteMirrorServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		f.requests = append(f.requests, req)
	}
	return stream.SendAndClose(&gitalypb.UpdateRemoteMirrorResponse{
		DivergentRefs: [][]byte{[]byte("refs/heads/main")},
	})
}

func TestMirrorSendsTheConnectionAndTheBranchPatterns(t *testing.T) {
	t.Parallel()

	fake := &fakeRemote{}
	fakes := newFakeServices()
	client := dialFake(t, func(srv *grpc.Server) {
		fakes.register(srv)
		gitalypb.RegisterRemoteServiceServer(srv, fake)
	})
	repo := client.Repo("shop.git")

	res, err := repo.Mirror(t.Context(), sdk.MirrorOptions{
		URL:                     "https://gitee.com/example/shop.git",
		Branches:                []string{"main", "release/*"},
		HTTPAuthorizationHeader: "Basic zzz",
		KeepDivergentRefs:       true,
	})
	require.NoError(t, err)
	require.Equal(t, []string{"refs/heads/main"}, res.DivergentRefs)

	require.Len(t, fake.requests, 3)
	first := fake.requests[0]
	require.NotNil(t, first.GetRepository())
	require.Equal(t, "https://gitee.com/example/shop.git", first.GetRemote().GetUrl())
	require.Equal(t, "Basic zzz", first.GetRemote().GetHttpAuthorizationHeader())
	require.True(t, first.GetKeepDivergentRefs())
	require.Empty(t, first.GetOnlyBranchesMatching())
	require.Equal(t, []byte("main"), fake.requests[1].GetOnlyBranchesMatching()[0])
	require.Equal(t, []byte("release/*"), fake.requests[2].GetOnlyBranchesMatching()[0])
}

func TestMirrorRefusesARequestWithoutAURL(t *testing.T) {
	t.Parallel()

	fakes := newFakeServices()
	client := dialFake(t, func(srv *grpc.Server) {
		fakes.register(srv)
		gitalypb.RegisterRemoteServiceServer(srv, &fakeRemote{})
	})

	_, err := client.Repo("shop.git").Mirror(t.Context(), sdk.MirrorOptions{})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, sdk.Code(err))
}
