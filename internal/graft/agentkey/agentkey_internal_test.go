package agentkey

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"

	"github.com/feder-cr/git-ai/internal/testhelper"
)

// failingReader stands for a random source that cannot produce bytes.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// withoutRandomness makes every nonce read fail for the duration of the test.
// The test must not run in parallel: randReader is a package variable, and a
// signature without a nonce is exactly what the other tests sign.
func withoutRandomness(t *testing.T) {
	t.Helper()

	old := randReader
	randReader = failingReader{err: errors.New("no entropy")}
	t.Cleanup(func() { randReader = old })
}

// failingSigner stands for a private key that cannot sign, for example one
// whose backing device disappeared.
type failingSigner struct{ pub ssh.PublicKey }

func (s failingSigner) PublicKey() ssh.PublicKey { return s.pub }

func (s failingSigner) Sign(io.Reader, []byte) (*ssh.Signature, error) {
	return nil, errors.New("key unavailable")
}

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()

	privateKey, _ := testhelper.AgentKeyPair(t)
	signer, err := ParsePrivateKey(privateKey)
	require.NoError(t, err)
	return signer
}

func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()

	_, publicKeyText := testhelper.AgentKeyPair(t)
	pub, err := ParsePublicKey([]byte(publicKeyText))
	require.NoError(t, err)
	return pub
}

// TestNonce_ReportsRandomSourceFailure checks that a failing random source is
// an error and not an empty nonce.
func TestNonce_ReportsRandomSourceFailure(t *testing.T) {
	withoutRandomness(t)

	nonce, err := Nonce()
	require.ErrorContains(t, err, "no entropy")
	require.Empty(t, nonce)
}

// TestSigner_RefusesToSignWithoutARandomSource checks that neither interceptor
// sends a request when the nonce cannot be drawn.
func TestSigner_RefusesToSignWithoutARandomSource(t *testing.T) {
	s := NewSigner(testSigner(t))
	withoutRandomness(t)

	ctx := context.Background()

	returned, err := s.sign(ctx, "/gitaly.RepositoryService/RepositoryExists", "")
	require.Error(t, err)
	require.NotNil(t, returned, "the caller keeps a usable context")

	called := false
	err = s.UnaryInterceptor()(ctx, "/gitaly.RepositoryService/RepositoryExists", nil, nil, nil,
		func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
			called = true
			return nil
		})
	require.Error(t, err)
	require.False(t, called, "an unsigned request is not sent")

	_, err = s.StreamInterceptor()(ctx, &grpc.StreamDesc{}, nil, "/gitaly.SSHService/SSHUploadPack",
		func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string, ...grpc.CallOption) (grpc.ClientStream, error) {
			called = true
			return nil, nil
		})
	require.Error(t, err)
	require.False(t, called, "an unsigned stream is not opened")
}

// TestSigner_RefusesToSignWhenTheKeyFails checks the same refusal when the
// key itself cannot sign.
func TestSigner_RefusesToSignWhenTheKeyFails(t *testing.T) {
	pub := testPublicKey(t)
	s := NewSigner(failingSigner{pub: pub})

	require.NotEmpty(t, s.Fingerprint())
	require.Equal(t, Fingerprint(pub), s.Fingerprint())

	ctx := context.Background()

	returned, err := s.sign(ctx, "/m", "")
	require.ErrorContains(t, err, "key unavailable")
	require.NotNil(t, returned, "the caller keeps a usable context")

	called := false
	err = s.UnaryInterceptor()(ctx, "/m", nil, nil, nil,
		func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
			called = true
			return nil
		})
	require.ErrorContains(t, err, "key unavailable")
	require.False(t, called, "an unsigned request is not sent")

	_, err = s.StreamInterceptor()(ctx, &grpc.StreamDesc{}, nil, "/m",
		func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string, ...grpc.CallOption) (grpc.ClientStream, error) {
			called = true
			return nil, nil
		})
	require.ErrorContains(t, err, "key unavailable")
	require.False(t, called, "an unsigned stream is not opened")
}

// TestFormatTimestamp checks that the timestamp travels as a decimal number.
func TestFormatTimestamp(t *testing.T) {
	require.Equal(t, "1700000000", formatTimestamp(1700000000))
	require.Equal(t, "0", formatTimestamp(0))
	require.Equal(t, "-1", formatTimestamp(-1))
}
