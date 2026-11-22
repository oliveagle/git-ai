package cachetier

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"

	gitalyauth "github.com/feder-cr/git-ai/auth"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	grpcclient "github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/internal/grpc/listenmux"
	"github.com/feder-cr/git-ai/internal/grpc/middleware/panichandler"
	"github.com/feder-cr/git-ai/internal/grpc/sidechannel"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/proxy"

	"github.com/feder-cr/git-ai/internal/graft/agentkey"
)

// DialUpstream dials addr for use as the upstream of a cache tier. The
// connection forces the proxy codec, which lets proxied RPCs forward raw
// frames unchanged. It also speaks the sidechannel protocol and proxies
// sidechannels, as Praefect does, so that sidechannel RPCs such as
// SSHUploadPackWithSidechannel pass through the tier. Typed calls work on the
// same connection.
func DialUpstream(ctx context.Context, addr string, logger log.Logger) (*grpc.ClientConn, error) {
	registry := sidechannel.NewRegistry()

	return grpcclient.New(ctx, addr,
		grpcclient.WithHandshaker(sidechannel.NewClientHandshaker(logger, registry)),
		grpcclient.WithGrpcOptions([]grpc.DialOption{
			grpc.WithDefaultCallOptions(grpc.ForceCodec(proxy.NewCodec())),
			grpc.WithChainUnaryInterceptor(sidechannel.NewUnaryProxy(registry, logger)),
			grpc.WithChainStreamInterceptor(sidechannel.NewStreamProxy(registry, logger)),
		}),
	)
}

// NewGRPCServer returns a gRPC server that serves ObjectService from the cache
// and transparently proxies every other RPC to the upstream. It also serves the
// standard gRPC health service without proxying it.
func NewGRPCServer(s *Server) *grpc.Server {
	// Accept backchannel connections, as Praefect does, so that clients which
	// dial with sidechannel support can use the tier. The proxy then opens their
	// sidechannels towards the upstream.
	lm := listenmux.New(insecure.NewCredentials())
	lm.Register(backchannel.NewServerHandshaker(s.logger, backchannel.NewRegistry(), nil))

	srv := grpc.NewServer(
		grpc.Creds(lm),
		grpc.ForceServerCodec(proxy.NewCodec()),
		grpc.UnknownServiceHandler(proxy.TransparentHandler(s.director)),
		// A panic in one request, for example on a damaged cache entry, fails that
		// request instead of killing the process and every other request with it.
		grpc.ChainUnaryInterceptor(panichandler.UnaryPanicHandler(s.logger)),
		grpc.ChainStreamInterceptor(panichandler.StreamPanicHandler(s.logger)),
	)

	gitalypb.RegisterObjectServiceServer(srv, s)

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthServer)

	return srv
}

// director routes a proxied call to the upstream. It peeks at the first request
// frame, which the proxy then forwards unchanged.
func (s *Server) director(ctx context.Context, _ string, peeker proxy.StreamPeeker) (*proxy.StreamParameters, error) {
	frame, err := peeker.Peek()
	if err != nil {
		return nil, structerr.NewInternal("peek first request frame: %w", err)
	}

	outCtx, err := s.outgoingContext(ctx)
	if err != nil {
		return nil, err
	}

	s.proxiedCalls.Add(1)
	return proxy.NewStreamParameters(proxy.Destination{
		Ctx:  outCtx,
		Conn: s.upstreamConn,
		Msg:  frame,
	}, nil, nil, nil), nil
}

// outgoingContext returns the context for an upstream call that forwards the
// client request carrying ctx unchanged. All of the client's incoming
// metadata is forwarded, so authorization, correlation IDs, feature flags and
// the signature of an agent reach the upstream. If the client sent no
// credential, the cache's own token is used when one is configured. A client
// that signs its requests with its agent key sends no authorization at all:
// its signature is forwarded instead, and the cache adds no token of its own,
// so that the upstream sees the agent and not the cache.
func (s *Server) outgoingContext(ctx context.Context) (context.Context, error) {
	incoming, _ := metadata.FromIncomingContext(ctx)
	outgoing := incoming.Copy()

	if len(outgoing.Get("authorization")) == 0 && !hasAgentSignature(outgoing) && s.token != "" {
		if err := s.addToken(ctx, outgoing); err != nil {
			return nil, err
		}
	}

	return metadata.NewOutgoingContext(ctx, outgoing), nil
}

// hasAgentSignature reports whether md carries the signature of an agent.
func hasAgentSignature(md metadata.MD) bool {
	return len(md.Get(agentkey.SignatureMetadata)) > 0 || len(md.Get(agentkey.KeyMetadata)) > 0
}

// addToken sets the cache's own token on outgoing.
func (s *Server) addToken(ctx context.Context, outgoing metadata.MD) error {
	md, err := s.tokenMetadata(ctx)
	if err != nil {
		return err
	}
	for key, values := range md {
		outgoing.Set(key, values)
	}
	return nil
}

// syntheticContext returns the context for an upstream call the tier makes on
// the caller's behalf, such as the RepositoryExists that authorizes a cached
// read or the batched fill of missing objects. Those calls are not the
// client's request: a signature covers the client's method and request, so it
// cannot authenticate them. When the caller signed with an agent key the tier
// therefore authenticates them with its own token, and reports
// errNoTierCredential when it has none. Every other caller keeps the behavior
// of outgoingContext: the client's authorization, or the tier's token when the
// client sent none.
func (s *Server) syntheticContext(ctx context.Context) (context.Context, error) {
	incoming, _ := metadata.FromIncomingContext(ctx)
	if !hasAgentSignature(incoming) {
		return s.outgoingContext(ctx)
	}

	if s.token == "" {
		return nil, errNoTierCredential
	}

	outgoing := incoming.Copy()
	for _, key := range []string{
		agentkey.KeyMetadata,
		agentkey.TimestampMetadata,
		agentkey.NonceMetadata,
		agentkey.SignatureMetadata,
	} {
		delete(outgoing, key)
	}
	if err := s.addToken(ctx, outgoing); err != nil {
		return nil, err
	}

	return metadata.NewOutgoingContext(ctx, outgoing), nil
}

// errNoTierCredential is returned when the tier has to make an upstream request
// on behalf of a signed caller but has no token of its own. The read is then
// forwarded to the upstream instead of being served from the cache.
var errNoTierCredential = errors.New("the cache tier needs its own token to serve a request signed by an agent: the signature cannot be replayed for the request the tier makes")

func (s *Server) tokenMetadata(ctx context.Context) (map[string]string, error) {
	md, err := gitalyauth.RPCCredentialsV2(s.token).GetRequestMetadata(ctx)
	if err != nil {
		return nil, structerr.NewInternal("sign upstream request: %w", err)
	}
	return md, nil
}
