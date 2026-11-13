package sdk

import (
	"context"
	"errors"
	"io"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// ReferenceValue returns the object ID that the reference with the full name
// name points at, read by its exact name: no revision rule applies, so that a
// branch named like it (refs/heads/refs/graft/green/main) is not taken for it.
// It returns an empty string when the reference does not exist.
//
// An agent token may not read the references the server reserves for the
// server itself (refs/graft/); the call fails with PermissionDenied then.
func (r *Repo) ReferenceValue(ctx context.Context, name string) (string, error) {
	if !strings.HasPrefix(name, "refs/") {
		return "", status.Errorf(codes.InvalidArgument, "reference %q: want a full name below refs/", name)
	}

	stream, err := gitalypb.NewRefServiceClient(r.client.conn).ListRefs(ctx, &gitalypb.ListRefsRequest{
		Repository: r.repository(),
		Patterns:   [][]byte{[]byte(name)},
	})
	if err != nil {
		return "", err
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		for _, ref := range resp.GetReferences() {
			if string(ref.GetName()) == name {
				return ref.GetTarget(), nil
			}
		}
	}
}

// UpdateReference moves the reference with the full name name from old to new,
// with a compare-and-swap: the update fails when the reference does not point
// at old, so that a concurrent move is never overwritten. An empty old means
// that the reference must not exist yet.
//
// Like Create, it needs the shared token: the references the server reserves
// for itself (refs/graft/) are the trust anchors of the branch management
// policy, and only an operator may move them.
func (r *Repo) UpdateReference(ctx context.Context, name, old, new string) error {
	if !strings.HasPrefix(name, "refs/") {
		return status.Errorf(codes.InvalidArgument, "reference %q: want a full name below refs/", name)
	}
	if new == "" {
		return status.Error(codes.InvalidArgument, "the new object ID is required")
	}
	if old == "" {
		zero, err := r.ZeroOID(ctx)
		if err != nil {
			return err
		}
		old = zero
	}

	stream, err := gitalypb.NewRefServiceClient(r.client.conn).UpdateReferences(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&gitalypb.UpdateReferencesRequest{
		Repository: r.repository(),
		Updates: []*gitalypb.UpdateReferencesRequest_Update{{
			Reference:   []byte(name),
			OldObjectId: []byte(old),
			NewObjectId: []byte(new),
		}},
	}); err != nil {
		return err
	}
	_, err = stream.CloseAndRecv()
	return err
}
