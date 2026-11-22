package testhelper

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// NewAgentKeyPair returns a new SSH key pair: the private key as a
// PEM-encoded OpenSSH key, and the public key in the format ssh-keygen writes.
// It does not need a testing.TB, so a test binary can generate credentials in
// TestMain; AgentKeyPair wraps it for the tests that run inside a test.
func NewAgentKeyPair() (privateKey []byte, publicKey string, err error) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}

	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return nil, "", err
	}

	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return nil, "", err
	}

	return pem.EncodeToMemory(block), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

// AgentKeyPair returns a new SSH key pair for the tests: the private key as a
// PEM-encoded OpenSSH key, and the public key in the format ssh-keygen writes.
// The public key is the credential of the agent, the private key signs its
// requests.
func AgentKeyPair(t testing.TB) (privateKey []byte, publicKey string) {
	t.Helper()

	privateKey, publicKey, err := NewAgentKeyPair()
	require.NoError(t, err)

	return privateKey, publicKey
}

// AgentKeyFiles returns a new SSH key pair for the tests, with the private key
// written to a file: the path of the file, and the public key.
func AgentKeyFiles(t testing.TB) (privateKeyPath, publicKey string) {
	t.Helper()

	privateKey, publicKey := AgentKeyPair(t)
	path := filepath.Join(t.TempDir(), "agent-key")
	require.NoError(t, os.WriteFile(path, privateKey, 0o600))

	return path, publicKey
}
