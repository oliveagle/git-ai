package conformance

import (
	"os"
	"testing"

	"github.com/feder-cr/git-ai/internal/testhelper"
)

func TestMain(m *testing.M) {
	if os.Getenv(fetchTransportEnv) != "" {
		os.Exit(runFetchTransport())
	}
	testhelper.Run(m)
}
