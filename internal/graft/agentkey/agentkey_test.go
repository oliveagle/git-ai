package agentkey_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/metadata"

	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/feder-cr/git-ai/internal/testhelper"
	gitalypb "github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()

	privateKey, publicKeyText := testhelper.AgentKeyPair(t)
	privateKey2, _ := agentkey.ParsePrivateKey(privateKey)
	publicKey, err := agentkey.ParsePublicKey([]byte(publicKeyText))
	require.NoError(t, err)
	require.Equal(t, publicKeyText, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(publicKey))))

	nonce, err := agentkey.Nonce()
	require.NoError(t, err)
	payload := agentkey.Payload(time.Now().Unix(), nonce, "/gitaly.OperationService/UserLand", "body-hash")

	signature, err := agentkey.Sign(privateKey2, payload)
	require.NoError(t, err)
	require.NoError(t, agentkey.Verify(publicKey, payload, signature))
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	t.Parallel()

	privateKey, publicKeyText := testhelper.AgentKeyPair(t)
	signer, err := agentkey.ParsePrivateKey(privateKey)
	require.NoError(t, err)
	publicKey, err := agentkey.ParsePublicKey([]byte(publicKeyText))
	require.NoError(t, err)

	payload := agentkey.Payload(1700000000, "nonce", "/gitaly.OperationService/UserLand", "body-hash")
	signature, err := agentkey.Sign(signer, payload)
	require.NoError(t, err)

	tampered := agentkey.Payload(1700000000, "nonce", "/gitaly.OperationService/UserCommitFiles", "body-hash")
	require.Error(t, agentkey.Verify(publicKey, tampered, signature))
}

func TestVerifyRejectsAnotherKey(t *testing.T) {
	t.Parallel()

	privateKey, _ := testhelper.AgentKeyPair(t)
	_, otherPublicText := testhelper.AgentKeyPair(t)

	signer, err := agentkey.ParsePrivateKey(privateKey)
	require.NoError(t, err)
	otherPublic, err := agentkey.ParsePublicKey([]byte(otherPublicText))
	require.NoError(t, err)

	payload := agentkey.Payload(1700000000, "nonce", "/m", "")
	signature, err := agentkey.Sign(signer, payload)
	require.NoError(t, err)
	require.Error(t, agentkey.Verify(otherPublic, payload, signature))
}

func TestFingerprintMatchesSSHKeygenFormat(t *testing.T) {
	t.Parallel()

	_, publicKeyText := testhelper.AgentKeyPair(t)
	publicKey, err := agentkey.ParsePublicKey([]byte(publicKeyText))
	require.NoError(t, err)

	fingerprint := agentkey.Fingerprint(publicKey)
	require.Equal(t, ssh.FingerprintSHA256(publicKey), fingerprint)
	require.True(t, strings.HasPrefix(fingerprint, "SHA256:"))
	require.NotContains(t, fingerprint, "=", "the padding of the base64 must be trimmed")

	again, err := agentkey.ParsePublicKey([]byte(publicKeyText))
	require.NoError(t, err)
	require.Equal(t, fingerprint, agentkey.Fingerprint(again), "the fingerprint must be stable")
}

func TestPayloadIsStable(t *testing.T) {
	t.Parallel()

	// The payload is part of the wire protocol: a deployed client and a
	// deployed server must build the same bytes. Changing this test means
	// changing PayloadVersion.
	got := string(agentkey.Payload(1700000000, "abcdef", "/gitaly.OperationService/UserLand", "hash"))
	require.Equal(t, "graft-ssh-v1\n1700000000\nabcdef\n/gitaly.OperationService/UserLand\nhash\n", got)
}

func TestNonceIsRandomAndWellFormed(t *testing.T) {
	t.Parallel()

	first, err := agentkey.Nonce()
	require.NoError(t, err)
	second, err := agentkey.Nonce()
	require.NoError(t, err)

	require.Len(t, first, agentkey.NonceBytes*2, "the nonce is hex")
	require.NotEqual(t, first, second)
}

func TestParsePublicKeyRejectsGarbage(t *testing.T) {
	t.Parallel()

	_, err := agentkey.ParsePublicKey([]byte("not a key"))
	require.Error(t, err)
}

func TestParsePrivateKeyRejectsGarbage(t *testing.T) {
	t.Parallel()

	_, err := agentkey.ParsePrivateKey([]byte("not a key"))
	require.Error(t, err)
}

func TestSignatureFromMetadata(t *testing.T) {
	t.Parallel()

	t.Run("no metadata", func(t *testing.T) {
		signature, err := agentkey.SignatureFromMetadata(context.Background())
		require.NoError(t, err)
		require.Nil(t, signature)
	})

	t.Run("complete signature", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			agentkey.KeyMetadata, "SHA256:abc",
			agentkey.TimestampMetadata, "1700000000",
			agentkey.NonceMetadata, "nonce",
			agentkey.SignatureMetadata, "c2ln",
		))
		signature, err := agentkey.SignatureFromMetadata(ctx)
		require.NoError(t, err)
		require.Equal(t, "SHA256:abc", signature.Fingerprint)
		require.Equal(t, int64(1700000000), signature.Timestamp)
		require.Equal(t, "nonce", signature.Nonce)
		require.Equal(t, "c2ln", signature.Value)
	})

	t.Run("incomplete signature is an error, never ignored", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			agentkey.KeyMetadata, "SHA256:abc",
			agentkey.TimestampMetadata, "1700000000",
		))
		_, err := agentkey.SignatureFromMetadata(ctx)
		require.ErrorContains(t, err, "incomplete")
	})

	t.Run("bad timestamp", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			agentkey.KeyMetadata, "SHA256:abc",
			agentkey.TimestampMetadata, "soon",
			agentkey.NonceMetadata, "nonce",
			agentkey.SignatureMetadata, "c2ln",
		))
		_, err := agentkey.SignatureFromMetadata(ctx)
		require.ErrorContains(t, err, "timestamp")
	})
}

func TestBodyHash(t *testing.T) {
	t.Parallel()

	req := &gitalypb.RepositoryExistsRequest{Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "a.git"}}
	first := agentkey.BodyHash(req)
	require.NotEmpty(t, first)

	same := &gitalypb.RepositoryExistsRequest{Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "a.git"}}
	require.Equal(t, first, agentkey.BodyHash(same), "the hash must be deterministic")

	other := &gitalypb.RepositoryExistsRequest{Repository: &gitalypb.Repository{StorageName: "default", RelativePath: "b.git"}}
	require.NotEqual(t, first, agentkey.BodyHash(other))

	require.Empty(t, agentkey.BodyHash("not a proto message"))
}
