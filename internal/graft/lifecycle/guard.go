// Package lifecycle guards the lifecycle of the repositories of a Graft
// server: a repository is never removed, it is archived and frozen, and only a
// super admin deletes it, once the archive retention has passed
// (doc/graft/PLAN13.md).
//
// The guard refuses the removal of a repository in every mode of the server.
// The removal of a repository destroys its history at once, and a mistake (a
// wrong path, a script that loops over the wrong names) is not recoverable:
// there is no undo, and the objects of the repository are gone from the bucket
// gc_grace later. The archive is the safe way to retire a repository: it keeps
// the repository, frozen and readable, for the archive retention, so that a
// mistake can still be undone, and it hands the deletion to a super admin
// alone.
package lifecycle

import (
	"context"

	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
)

// removeRepositoryMethod is the method of the RPC that removes a repository.
const removeRepositoryMethod = "/gitaly.RepositoryService/RemoveRepository"

// Guard refuses the removal of a repository.
type Guard struct {
	logger log.Logger
}

// New returns the guard.
func New(logger log.Logger) *Guard {
	return &Guard{logger: logger}
}

// UnaryInterceptor returns the unary interceptor: it refuses the removal of a
// repository. Every other RPC is left to its handler.
func (g *Guard) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == removeRepositoryMethod {
			return nil, g.refuse(ctx)
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor returns the stream interceptor. The removal of a
// repository is not a streaming RPC, so nothing is refused here.
func (g *Guard) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, stream)
	}
}

// refuse refuses the removal of a repository and records it: a refused
// removal is an attempt to destroy a repository, and the operator wants to
// know about it.
func (g *Guard) refuse(ctx context.Context) error {
	g.logger.WithField("method", removeRepositoryMethod).WarnContext(ctx,
		"graft: the removal of a repository was refused: a repository is archived, never removed")
	return structerr.NewFailedPrecondition("removing a repository is not supported: " +
		"archive it instead (graft storage archive), and let a super admin purge it once the archive retention has passed (graft storage purge)")
}
