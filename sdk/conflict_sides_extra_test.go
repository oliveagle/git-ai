package sdk_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestConflictSidesReportsEachSideItCannotRead covers the package whose sides
// the repository does not hold: a side the merge marked unavailable, a side
// whose blob is missing, an empty side, and the proposals of the resolver.
func TestConflictSidesReportsEachSideItCannotRead(t *testing.T) {
	fs := newFakeServices()
	fs.objects.objects = map[string][]byte{
		"base-oid":  []byte("base\n"),
		"empty-oid": nil,
		"prop-oid":  []byte("proposal\n"),
	}
	client := dialFake(t, fs.register)
	repo := client.Repo("repo")

	sides, err := repo.ConflictSides(context.Background(), &gitalypb.LandConflict{
		Files: []*gitalypb.LandConflictFile{
			{
				Path:   []byte("a.txt"),
				Base:   &gitalypb.LandConflictSide{Oid: "base-oid"},
				Ours:   &gitalypb.LandConflictSide{Oid: "gone-oid"},
				Theirs: &gitalypb.LandConflictSide{Unavailable: "the merge did not keep it"},
			},
			{
				Path: []byte("b.txt"),
				Base: &gitalypb.LandConflictSide{Oid: "empty-oid"},
			},
			{
				Path:     []byte("c.txt"),
				Proposal: &gitalypb.LandConflictProposal{Oid: "prop-oid", Mode: "100644", Resolver: "rules"},
			},
			{
				Path:     []byte("d.txt"),
				Proposal: &gitalypb.LandConflictProposal{Action: "delete", Resolver: "model"},
			},
			{
				Path:     []byte("e.txt"),
				Proposal: &gitalypb.LandConflictProposal{Oid: "gone-oid", Mode: "100755"},
			},
			{
				Path:     []byte("f.txt"),
				Proposal: &gitalypb.LandConflictProposal{Mode: "100644"},
			},
		},
	})
	require.NoError(t, err)

	require.Equal(t, []byte("base\n"), sides["a.txt"].Base)
	require.Nil(t, sides["a.txt"].Ours)
	require.Contains(t, sides["a.txt"].Unreadable, "ours")
	require.Contains(t, sides["a.txt"].Unreadable, "theirs")
	require.Equal(t, "the merge did not keep it", sides["a.txt"].Unreadable["theirs"])

	// An object the repository holds with no content is an empty side, not an
	// unreadable one.
	require.NotNil(t, sides["b.txt"].Base)
	require.Empty(t, sides["b.txt"].Base)
	require.Empty(t, sides["b.txt"].Unreadable)

	require.Equal(t, []byte("proposal\n"), sides["c.txt"].Proposal.Content)
	require.Equal(t, "100644", sides["c.txt"].Proposal.Mode)
	require.True(t, sides["d.txt"].Proposal.Delete)
	require.Empty(t, sides["d.txt"].Unreadable)

	// A proposal whose blob is gone, and one that names no blob at all, are
	// unreadable proposals.
	require.Contains(t, sides["e.txt"].Unreadable, "proposal")
	require.Contains(t, sides["f.txt"].Unreadable, "proposal")
}

// failingObjects fails every GetObjects call with a failure that is not
// transient, so the SDK reports it instead of retrying.
type failingObjects struct {
	gitalypb.UnimplementedObjectServiceServer
}

func (failingObjects) GetObjects(*gitalypb.GetObjectsRequest, gitalypb.ObjectService_GetObjectsServer) error {
	return status.Error(codes.Internal, "the object store is unavailable")
}

// TestConflictSidesReportsAFailedRead covers the read of the sides that fails
// as a whole: the error is reported and no side is guessed.
func TestConflictSidesReportsAFailedRead(t *testing.T) {
	client := dialFake(t, func(srv *grpc.Server) {
		gitalypb.RegisterObjectServiceServer(srv, failingObjects{})
	})

	_, err := client.Repo("repo").ConflictSides(context.Background(), &gitalypb.LandConflict{
		Files: []*gitalypb.LandConflictFile{{
			Path: []byte("a.txt"),
			Base: &gitalypb.LandConflictSide{Oid: "base-oid"},
		}},
	})
	require.ErrorContains(t, err, "the object store is unavailable")
}

// TestZeroOIDFollowsTheObjectFormat covers the zero object ID of a repository:
// forty zeros for SHA-1, sixty-four for SHA-256, and a refusal for a format the
// SDK does not know.
func TestZeroOIDFollowsTheObjectFormat(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format gitalypb.ObjectFormat
		want   string
		err    bool
	}{
		{name: "sha1", format: gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1, want: "0000000000000000000000000000000000000000"},
		{name: "sha256", format: gitalypb.ObjectFormat_OBJECT_FORMAT_SHA256, want: "0000000000000000000000000000000000000000000000000000000000000000"},
		{name: "unknown", format: gitalypb.ObjectFormat(99), err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := dialFake(t, func(srv *grpc.Server) {
				gitalypb.RegisterRepositoryServiceServer(srv, &fakeObjectFormat{format: tc.format})
			})
			got, err := client.Repo("repo").ZeroOID(context.Background())
			if tc.err {
				require.ErrorContains(t, err, "unknown object format")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// fakeObjectFormat answers the object format of a repository.
type fakeObjectFormat struct {
	gitalypb.UnimplementedRepositoryServiceServer
	format gitalypb.ObjectFormat
}

func (f *fakeObjectFormat) ObjectFormat(context.Context, *gitalypb.ObjectFormatRequest) (*gitalypb.ObjectFormatResponse, error) {
	return &gitalypb.ObjectFormatResponse{Format: f.format}, nil
}

// TestResolveRevisionRefusesAnEmptyRevision covers the revision the caller
// left out: it is refused before the server is asked.
func TestResolveRevisionRefusesAnEmptyRevision(t *testing.T) {
	client := dialFake(t, func(*grpc.Server) {})
	_, err := client.Repo("repo").ResolveRevision(context.Background(), "")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestReferenceExistsRefusesANameOutsideRefs covers the reference name that is
// not a full name below refs/, which the server would answer about anyway.
func TestReferenceExistsRefusesANameOutsideRefs(t *testing.T) {
	client := dialFake(t, func(*grpc.Server) {})
	_, err := client.Repo("repo").ReferenceExists(context.Background(), "heads/main")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "want a full name below refs/")
}

// fakeRefs answers ListRefs with the references it holds.
type fakeRefs struct {
	gitalypb.UnimplementedRefServiceServer
	refs [][]byte
}

func (f *fakeRefs) ListRefs(_ *gitalypb.ListRefsRequest, stream gitalypb.RefService_ListRefsServer) error {
	if len(f.refs) == 0 {
		return nil
	}
	return stream.Send(&gitalypb.ListRefsResponse{References: []*gitalypb.ListRefsResponse_Reference{{Name: f.refs[0]}}})
}

// TestHasReferencesReadsTheFirstReference covers the two answers of the probe:
// an empty repository, and one that holds a reference.
func TestHasReferencesReadsTheFirstReference(t *testing.T) {
	empty := dialFake(t, func(srv *grpc.Server) { gitalypb.RegisterRefServiceServer(srv, &fakeRefs{}) })
	has, err := empty.Repo("repo").HasReferences(context.Background())
	require.NoError(t, err)
	require.False(t, has)

	full := dialFake(t, func(srv *grpc.Server) { gitalypb.RegisterRefServiceServer(srv, &fakeRefs{refs: [][]byte{[]byte("refs/heads/main")}}) })
	has, err = full.Repo("repo").HasReferences(context.Background())
	require.NoError(t, err)
	require.True(t, has)
}
