package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/agentkey"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/metadata"
)

func TestMain(m *testing.M) {
	testhelper.Run(m)
}

// agentKey is the key pair of a test agent: the private key signs the
// requests, the public key is the credential the server knows.
type agentKey struct {
	name        string
	signer      ssh.Signer
	fingerprint string
	publicKey   string
}

// newAgentKey returns the key pair of a test agent.
func newAgentKey(t testing.TB, name string) *agentKey {
	t.Helper()

	_, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	signer, err := ssh.NewSignerFromKey(private)
	require.NoError(t, err)

	return &agentKey{
		name:        name,
		signer:      signer,
		fingerprint: agentkey.Fingerprint(signer.PublicKey()),
		publicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))),
	}
}

// agent returns the configuration of the agent.
func (k *agentKey) agent() config.GraftAgent {
	return config.GraftAgent{Name: k.name, PublicKey: k.publicKey}
}

// sign returns the metadata of a request the agent signed.
func (k *agentKey) sign(t testing.TB, method, bodyHash string) metadata.MD {
	t.Helper()

	return k.signAt(t, method, bodyHash, time.Now())
}

// signAt returns the metadata of a request the agent signed at the given
// time.
func (k *agentKey) signAt(t testing.TB, method, bodyHash string, at time.Time) metadata.MD {
	t.Helper()

	nonce, err := agentkey.Nonce()
	require.NoError(t, err)

	timestamp := at.Unix()
	signature, err := agentkey.Sign(k.signer, agentkey.Payload(timestamp, nonce, method, bodyHash))
	require.NoError(t, err)

	return metadata.Pairs(
		agentkey.KeyMetadata, k.fingerprint,
		agentkey.TimestampMetadata, strconv.FormatInt(timestamp, 10),
		agentkey.NonceMetadata, nonce,
		agentkey.SignatureMetadata, signature,
	)
}

// contextWithMetadata returns a context with the given incoming metadata.
func contextWithMetadata(t testing.TB, md metadata.MD) context.Context {
	t.Helper()

	return metadata.NewIncomingContext(testhelper.Context(t), md)
}

// contextWithSignature returns a context with the metadata of a signed
// request, and with the authorization header when it is not empty.
func contextWithSignature(t testing.TB, md metadata.MD, authorization string) context.Context {
	t.Helper()

	if md == nil {
		md = metadata.MD{}
	}
	if authorization != "" {
		md.Set("authorization", authorization)
	}

	return metadata.NewIncomingContext(testhelper.Context(t), md)
}

// signedAgentContext returns the context of a request the agent signed for the given
// method. Every call needs its own context: a signature may be used once.
func signedAgentContext(t testing.TB, key *agentKey, method string) context.Context {
	t.Helper()

	return contextWithSignature(t, key.sign(t, method, ""), "")
}

// signedAgentContextFor returns the context of a request the agent signed: the
// signature covers the method and the hash of the request message.
func signedAgentContextFor(t testing.TB, key *agentKey, method string, req any) context.Context {
	t.Helper()

	return contextWithSignature(t, key.sign(t, method, agentkey.BodyHash(req)), "")
}
