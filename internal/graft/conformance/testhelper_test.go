package conformance

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
)

// The agents of the harness all share one key pair: the private key signs the
// requests of the stock client and of the RPC tables, the public key is the one
// the backends configure their agent with. The pair is generated in TestMain
// because agentGraft does not take a testing.TB.
var (
	agentPrivateKeyPath string
	agentPublicKey      string
)

func TestMain(m *testing.M) {
	if os.Getenv(fetchTransportEnv) != "" {
		os.Exit(runFetchTransport())
	}

	privateKey, publicKey, err := testhelper.NewAgentKeyPair()
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: generate the agent key pair: %v\n", err)
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "gitai-conformance-agent-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "conformance: create the agent key directory: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	agentPrivateKeyPath = filepath.Join(dir, "agent-key")
	if err := os.WriteFile(agentPrivateKeyPath, privateKey, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "conformance: write the agent private key: %v\n", err)
		os.Exit(1)
	}
	agentPublicKey = publicKey

	testhelper.Run(m)
}
