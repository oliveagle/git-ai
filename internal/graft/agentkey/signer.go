package agentkey

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Signer signs the requests of an agent with its private key. The client
// installs its interceptors on the connection, and every request it makes
// then carries a signature the server verifies (doc/graft/PLAN14.md).
type Signer struct {
	signer      ssh.Signer
	fingerprint string
}

// NewSigner returns a signer that signs with the private key of an agent.
func NewSigner(signer ssh.Signer) *Signer {
	return &Signer{signer: signer, fingerprint: Fingerprint(signer.PublicKey())}
}

// Fingerprint returns the fingerprint of the public key of the agent, the one
// the server looks the agent up with.
func (s *Signer) Fingerprint() string {
	return s.fingerprint
}

// UnaryInterceptor returns the client interceptor that signs every unary
// request: the signature covers the method and the hash of the request. A
// request that cannot be signed is not sent: an agent never sends a request
// without the signature that carries its identity.
func (s *Signer) UnaryInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		signed, err := s.sign(ctx, method, BodyHash(req))
		if err != nil {
			return err
		}
		return invoker(signed, method, req, reply, cc, opts...)
	}
}

// StreamInterceptor returns the client interceptor that signs every streaming
// request. The message of the stream is not known when the request is signed,
// so the signature covers the method alone. A request that cannot be signed is
// not sent.
func (s *Signer) StreamInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		signed, err := s.sign(ctx, method, "")
		if err != nil {
			return nil, err
		}
		return streamer(signed, desc, cc, method, opts...)
	}
}

// sign signs the request and returns a context that carries the signature. It
// fails rather than return an unsigned context: the caller must not send a
// request that carries no signature.
func (s *Signer) sign(ctx context.Context, method, bodyHash string) (context.Context, error) {
	timestamp := time.Now().Unix()
	nonce, err := Nonce()
	if err != nil {
		return ctx, fmt.Errorf("sign the request: %w", err)
	}

	signature, err := Sign(s.signer, Payload(timestamp, nonce, method, bodyHash))
	if err != nil {
		return ctx, err
	}

	return metadata.AppendToOutgoingContext(ctx,
		KeyMetadata, s.fingerprint,
		TimestampMetadata, formatTimestamp(timestamp),
		NonceMetadata, nonce,
		SignatureMetadata, signature,
	), nil
}

// BodyHash returns the hash of the request message: the SHA-256 of its
// deterministic serialization, in hex. It is empty for a message that is not
// a protocol buffer message.
func BodyHash(msg any) string {
	message, ok := msg.(proto.Message)
	if !ok {
		return ""
	}

	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return ""
	}

	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
