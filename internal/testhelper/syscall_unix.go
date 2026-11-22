//go:build !windows

package testhelper

import (
	"fmt"
	"os"
	"syscall"

	"github.com/feder-cr/git-ai/internal/helper/perm"
)

// lockFileExclusive takes an exclusive advisory lock on f. It guards the
// write of an executable against a child process forking while the writeable
// descriptor is still open (ETXTBSY).
func lockFileExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// lockFileShared takes a shared advisory lock on f, blocking until every
// exclusive holder of the same file has closed its descriptor.
func lockFileShared(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_SH)
}

// processUmask returns the umask of the current process. It is computed once
// at initialization time because the umask is process-wide state that is
// unsafe to read and write while threads are running.
func processUmask() perm.Umask {
	oldMask := syscall.Umask(0)
	syscall.Umask(oldMask)
	return perm.Umask(oldMask)
}

// mustFindNoFinishedChildProcess reports a child process that has exited but
// was not reaped yet.
func mustFindNoFinishedChildProcess() error {
	// Wait4(pid int, wstatus *WaitStatus, options int, rusage *Rusage) (wpid int, err error)
	//
	// We use pid -1 to wait for any child. We don't care about wstatus or
	// rusage. Use WNOHANG to return immediately if there is no child waiting
	// to be reaped.
	wpid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
	if err == nil && wpid > 0 {
		return fmt.Errorf("wait4 found child process %d", wpid)
	}

	return nil
}
