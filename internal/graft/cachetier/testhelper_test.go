package cachetier

import (
	"io"
	"testing"

	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

func TestMain(m *testing.M) {
	logger, err := log.Configure(io.Discard, "text", "error")
	if err != nil {
		panic(err)
	}
	testLog = logger
	testhelper.Run(m)
}
