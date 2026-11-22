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

// ReferenceUpdate is one reference to move, with the compare-and-swap of the
// move: the update fails when the reference does not point at Old.
type ReferenceUpdate struct {
	// Name is the full name of the reference.
	Name string
	// Old is the object ID the reference must point at. It is empty when the
	// reference must not exist yet.
	Old string
	// New is the object ID the reference points at after the update.
	New string
}

// UpdateReferences moves a set of references atomically: either every
// reference is moved, or none is. It is what a publisher needs when the
// checkpoint of a branch and the tree it certifies have to move together: a
// reader that sees one of them and not the other sees a checkpoint that
// certifies no tree, never one that certifies a tree the checkpoint does not
// have.
//
// Like UpdateReference, it needs the shared token: the references the server
// reserves for itself (refs/graft/) are the trust anchors of the branch
// management policy, and only an operator may move them.
func (r *Repo) UpdateReferences(ctx context.Context, updates []ReferenceUpdate) error {
	if len(updates) == 0 {
		return status.Error(codes.InvalidArgument, "no reference to update")
	}

	zero, err := r.ZeroOID(ctx)
	if err != nil {
		return err
	}

	protoUpdates := make([]*gitalypb.UpdateReferencesRequest_Update, 0, len(updates))
	for _, update := range updates {
		if !strings.HasPrefix(update.Name, "refs/") {
			return status.Errorf(codes.InvalidArgument, "reference %q: want a full name below refs/", update.Name)
		}
		if update.New == "" {
			return status.Errorf(codes.InvalidArgument, "reference %q: the new object ID is required", update.Name)
		}
		old := update.Old
		if old == "" {
			old = zero
		}
		protoUpdates = append(protoUpdates, &gitalypb.UpdateReferencesRequest_Update{
			Reference:   []byte(update.Name),
			OldObjectId: []byte(old),
			NewObjectId: []byte(update.New),
		})
	}

	stream, err := gitalypb.NewRefServiceClient(r.client.conn).UpdateReferences(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&gitalypb.UpdateReferencesRequest{
		Repository: r.repository(),
		Updates:    protoUpdates,
	}); err != nil {
		return err
	}
	_, err = stream.CloseAndRecv()
	return err
}
