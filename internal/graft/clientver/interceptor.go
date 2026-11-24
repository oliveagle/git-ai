package clientver

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/version"
)

// MetadataKey is the gRPC metadata key a client reports its version in. A
// client built before the key existed reports nothing, and that absence is
// what tells the server it predates the contract.
const MetadataKey = "graft-client-version"

// unreportedVersion is what a build without a version reports: it is a client
// of this binary that was compiled without the release workflow's ldflags, so
// it is newer than the contract rather than older than any release.
const unreportedVersion = "unknown"

// refused counts the requests a server refused because the client was too old.
// The version itself is in the log line, not in a label: it is unbounded.
var refused = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_client_version_refused_total",
	Help: "Requests refused because the client is older than graft.min_client_version.",
})

// WithVersion marks ctx as coming from a client of this build. It always adds
// the key, even when the build carries no version, so that a server can tell a
// new client it cannot compare from a client that predates the contract.
func WithVersion(ctx context.Context) context.Context {
	v := version.GetVersion()
	if v == "" {
		v = unreportedVersion
	}
	return metadata.AppendToOutgoingContext(ctx, MetadataKey, v)
}

// Reported returns the version the caller reported and whether it reported one
// at all.
func Reported(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get(MetadataKey)
	if len(values) == 0 {
		return "", false
	}
	return values[0], true
}

// UnaryClientInterceptor reports the version of this build on every unary
// request.
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(WithVersion(ctx), method, req, reply, cc, opts...)
	}
}

// StreamClientInterceptor reports the version of this build on every stream.
func StreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		return streamer(WithVersion(ctx), desc, cc, method, opts...)
	}
}

// UnaryServerInterceptor refuses a unary request from a client older than min:
// one that reports a version below it, and one that reports no version at all,
// because a client that does not report one predates the contract. A build of
// this binary without a version reports "unknown", which cannot be compared
// and is allowed. Methods that answer what a server requires (ServerInfo and
// the health probes) are always answered, so that an old client can be told
// what is wrong and an orchestrator can still see the process is alive.
func UnaryServerInterceptor(min Version, logger log.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := allow(ctx, info.FullMethod, min, logger); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamServerInterceptor is UnaryServerInterceptor for streams.
func StreamServerInterceptor(min Version, logger log.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := allow(ss.Context(), info.FullMethod, min, logger); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

// exemptMethods are answered whatever the client reports: the health probes an
// orchestrator uses, and the version report a client reads before it decides
// whether the rest of the server will answer.
var exemptMethods = map[string]bool{
	"/grpc.health.v1.Health/Check":     true,
	"/grpc.health.v1.Health/Watch":     true,
	"/grpc.health.v1.Health/List":      true,
	"/gitaly.ServerService/ServerInfo": true,
}

// allow reports whether the call may proceed.
func allow(ctx context.Context, method string, min Version, logger log.Logger) error {
	if exemptMethods[method] {
		return nil
	}

	raw, reported := Reported(ctx)
	if !reported {
		refused.Inc()
		logger.WithField("method", method).
			WithField("min_client_version", min.String()).
			WarnContext(ctx, "graft: refusing a client that reports no version: it is older than the contract")
		return structerr.NewFailedPrecondition("the server requires client version %s or newer; this client reports no version, so it predates the contract: run `graft upgrade` to update the graft tools", min)
	}

	client, ok := Parse(raw)
	if !ok {
		// A build of this tree without the release ldflags: it cannot be
		// compared, and refusing it would refuse the server's own tools.
		logger.WithField("method", method).
			WithField("client_version", raw).
			InfoContext(ctx, "graft: a client reports a version that cannot be compared, answering it")
		return nil
	}
	if client.Compare(min) < 0 {
		refused.Inc()
		logger.WithField("method", method).
			WithField("client_version", raw).
			WithField("min_client_version", min.String()).
			WarnContext(ctx, "graft: refusing a client older than the minimum")
		return structerr.NewFailedPrecondition("the server requires client version %s or newer; this client is %s: run `graft upgrade` to update the graft tools, or install the package of this release", min, raw)
	}
	return nil
}
