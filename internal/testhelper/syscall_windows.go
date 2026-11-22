//go:build windows

package testhelper

import (
	"os"

	"github.com/feder-cr/git-ai/internal/helper/perm"
)

// Windows does not have flock or ETXTBSY: closing the writeable handle is
// enough to release an executable for execution.
func lockFileExclusive(*os.File) error { return nil }

func lockFileShared(*os.File) error { return nil }

// processUmask returns the umask of the current process. Windows has no umask.
func processUmask() perm.Umask { return 0 }

// mustFindNoFinishedChildProcess reports a child process that has exited but
// was not reaped yet. Windows has no wait4; the process leak checks are
// meaningful on the platforms where the server runs.
func mustFindNoFinishedChildProcess() error { return nil }
