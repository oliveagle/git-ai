//go:build !windows

package changelog

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// flockFile takes an exclusive advisory lock on f. When nonBlocking is set
// and another process holds the lock, it fails with an error for which
// isLockWouldBlock reports true.
func flockFile(f *os.File, nonBlocking bool) error {
	how := syscall.LOCK_EX
	if nonBlocking {
		how |= syscall.LOCK_NB
	}
	return syscall.Flock(int(f.Fd()), how)
}

func funlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func isLockWouldBlock(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// fileIdentity returns the device and inode that identify the file, so that a
// writer notices a log that was replaced or truncated.
func fileIdentity(_ *os.File, info fs.FileInfo) (device, inode uint64, ok bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), stat.Ino, true
}
