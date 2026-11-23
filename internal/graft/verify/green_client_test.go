package verify

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// fakeRefStream scripts the pages and the error of a ListRefs stream.
type fakeRefStream struct {
	grpc.ServerStreamingClient[gitalypb.ListRefsResponse]

	pages []*gitalypb.ListRefsResponse
	err   error
}

func (s *fakeRefStream) Recv() (*gitalypb.ListRefsResponse, error) {
	if len(s.pages) > 0 {
		page := s.pages[0]
		s.pages = s.pages[1:]
		return page, nil
	}
	if s.err != nil {
		err := s.err
		s.err = nil
		return nil, err
	}
	return nil, io.EOF
}

// fakeUpdateStream records the request of UpdateReferences and scripts the
// Send and CloseAndRecv errors.
type fakeUpdateStream struct {
	grpc.ClientStreamingClient[gitalypb.UpdateReferencesRequest, gitalypb.UpdateReferencesResponse]

	req      *gitalypb.UpdateReferencesRequest
	sendErr  error
	closeErr error
}

func (s *fakeUpdateStream) Send(req *gitalypb.UpdateReferencesRequest) error {
	s.req = req
	return s.sendErr
}

func (s *fakeUpdateStream) CloseAndRecv() (*gitalypb.UpdateReferencesResponse, error) {
	return &gitalypb.UpdateReferencesResponse{}, s.closeErr
}

// fakeRefService answers the two RPCs refUpdater uses with the scripted
// results, and records the requests.
type fakeRefService struct {
	gitalypb.RefServiceClient

	listStream   *fakeRefStream
	listErr      error
	updateStream *fakeUpdateStream
	updateErr    error
	gotList      *gitalypb.ListRefsRequest
}

func (f *fakeRefService) ListRefs(_ context.Context, in *gitalypb.ListRefsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[gitalypb.ListRefsResponse], error) {
	f.gotList = in
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listStream, nil
}

func (f *fakeRefService) UpdateReferences(_ context.Context, _ ...grpc.CallOption) (grpc.ClientStreamingClient[gitalypb.UpdateReferencesRequest, gitalypb.UpdateReferencesResponse], error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	return f.updateStream, nil
}

func newTestRefUpdater(refs gitalypb.RefServiceClient) *refUpdater {
	return &refUpdater{
		repository: &gitalypb.Repository{StorageName: "default", RelativePath: "repo"},
		refs:       refs,
	}
}

func TestRefUpdater_Current(t *testing.T) {
	t.Parallel()

	tests := []struct {
		desc    string
		svc     *fakeRefService
		ref     string
		want    string
		wantErr string
	}{
		{
			desc:    "a revision suffix is not a reference name",
			svc:     &fakeRefService{},
			ref:     "refs/heads/main~1",
			wantErr: "not a full reference name",
		},
		{
			desc: "a list error is reported",
			svc: &fakeRefService{
				listErr: errors.New("connection refused"),
			},
			ref:     "refs/heads/main",
			wantErr: "list refs/heads/main: connection refused",
		},
		{
			desc: "the reference is read by its exact name",
			svc: &fakeRefService{
				listStream: &fakeRefStream{pages: []*gitalypb.ListRefsResponse{{
					References: []*gitalypb.ListRefsResponse_Reference{
						{Name: []byte("refs/heads/main"), Target: "1111111111111111111111111111111111111111"},
					},
				}}},
			},
			ref:  "refs/heads/main",
			want: "1111111111111111111111111111111111111111",
		},
		{
			desc: "a page with another name is skipped and absence is empty",
			svc: &fakeRefService{
				listStream: &fakeRefStream{pages: []*gitalypb.ListRefsResponse{{
					References: []*gitalypb.ListRefsResponse_Reference{
						{Name: []byte("refs/heads/main-old"), Target: "2222222222222222222222222222222222222222"},
					},
				}}},
			},
			ref:  "refs/heads/main",
			want: "",
		},
		{
			desc: "a stream error is reported",
			svc: &fakeRefService{
				listStream: &fakeRefStream{err: errors.New("stream reset")},
			},
			ref:     "refs/heads/main",
			wantErr: "list refs/heads/main: stream reset",
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			updater := newTestRefUpdater(tc.svc)
			got, err := updater.Current(testhelper.Context(t), tc.ref)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
			if tc.svc.gotList != nil {
				require.Equal(t, [][]byte{[]byte(tc.ref)}, tc.svc.gotList.GetPatterns())
			}
		})
	}
}

func TestRefUpdater_Move(t *testing.T) {
	t.Parallel()

	const next = "3333333333333333333333333333333333333333"

	t.Run("a move without a new object ID is refused", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{})
		require.ErrorContains(t, updater.Move(testhelper.Context(t), "refs/graft/green/main", "", ""), "needs a new object ID")
	})

	t.Run("an update error is reported", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{updateErr: errors.New("unavailable")})
		require.ErrorContains(t, updater.Move(testhelper.Context(t), "refs/graft/green/main", "", next), "update refs/graft/green/main: unavailable")
	})

	t.Run("a send error is reported", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{updateStream: &fakeUpdateStream{sendErr: errors.New("broken pipe")}})
		require.ErrorContains(t, updater.Move(testhelper.Context(t), "refs/graft/green/main", "", next), "broken pipe")
	})

	t.Run("a close error is reported", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{updateStream: &fakeUpdateStream{closeErr: errors.New("ref changed")}})
		err := updater.Move(testhelper.Context(t), "refs/graft/green/main", "4444444444444444444444444444444444444444", next)
		require.ErrorContains(t, err, "update refs/graft/green/main from 4444444444444444444444444444444444444444 to "+next)
	})

	t.Run("the old value of a new reference is the zero object ID", func(t *testing.T) {
		t.Parallel()

		stream := &fakeUpdateStream{}
		updater := newTestRefUpdater(&fakeRefService{updateStream: stream})
		require.NoError(t, updater.Move(testhelper.Context(t), "refs/graft/green/main", "", next))
		require.NotNil(t, stream.req)
		updates := stream.req.GetUpdates()
		require.Len(t, updates, 1)
		require.Equal(t, []byte("refs/graft/green/main"), updates[0].GetReference())
		require.Equal(t, []byte("0000000000000000000000000000000000000000"), updates[0].GetOldObjectId())
		require.Equal(t, []byte(next), updates[0].GetNewObjectId())
	})
}

func TestRefUpdater_MoveAll(t *testing.T) {
	t.Parallel()

	const (
		next = "5555555555555555555555555555555555555555"
		ref  = "refs/graft/green/main"
	)

	t.Run("no move is no request", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{})
		require.NoError(t, updater.MoveAll(testhelper.Context(t), nil))
	})

	t.Run("a move without a new object ID is refused", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{})
		err := updater.MoveAll(testhelper.Context(t), []Move{{Ref: ref}})
		require.ErrorContains(t, err, "a new object ID is required")
	})

	t.Run("an update error is reported", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{updateErr: errors.New("unavailable")})
		err := updater.MoveAll(testhelper.Context(t), []Move{{Ref: ref, New: next}})
		require.ErrorContains(t, err, "update references: unavailable")
	})

	t.Run("a send error is reported", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{updateStream: &fakeUpdateStream{sendErr: errors.New("broken pipe")}})
		err := updater.MoveAll(testhelper.Context(t), []Move{{Ref: ref, New: next}})
		require.ErrorContains(t, err, "update references: broken pipe")
	})

	t.Run("a close error is reported", func(t *testing.T) {
		t.Parallel()

		updater := newTestRefUpdater(&fakeRefService{updateStream: &fakeUpdateStream{closeErr: errors.New("ref changed")}})
		err := updater.MoveAll(testhelper.Context(t), []Move{{Ref: ref, New: next}})
		require.ErrorContains(t, err, "update references: ref changed")
	})

	t.Run("an empty old value is the zero object ID of the new one", func(t *testing.T) {
		t.Parallel()

		stream := &fakeUpdateStream{}
		updater := newTestRefUpdater(&fakeRefService{updateStream: stream})
		require.NoError(t, updater.MoveAll(testhelper.Context(t), []Move{
			{Ref: ref, New: next},
			{Ref: "refs/graft/green/other", Old: "6666666666666666666666666666666666666666", New: next},
		}))
		require.NotNil(t, stream.req)
		updates := stream.req.GetUpdates()
		require.Len(t, updates, 2)
		require.Equal(t, []byte("0000000000000000000000000000000000000000"), updates[0].GetOldObjectId())
		require.Equal(t, []byte("6666666666666666666666666666666666666666"), updates[1].GetOldObjectId())
	})
}

// TestDialRefUpdater_RefusesAnUnusableAddress checks that a token-carrying
// ref updater reports a dial failure instead of returning a broken client.
func TestDialRefUpdater_RefusesAnUnusableAddress(t *testing.T) {
	t.Parallel()

	_, err := dialRefUpdater(testhelper.Context(t), "", "token", nil, "default")
	require.Error(t, err)
	require.ErrorContains(t, err, "dial ref service")
}
