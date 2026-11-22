//go:build windows

package changelog

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// flockFile takes an exclusive lock on f. When nonBlocking is set and another
// process holds the lock, it fails with an error for which isLockWouldBlock
// reports true.
func flockFile(f *os.File, nonBlocking bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonBlocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
}

func funlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

func isLockWouldBlock(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// fileIdentity returns the volume serial number and file index that identify
// the file, so that a writer notices a log that was replaced or truncated.
func fileIdentity(f *os.File, _ fs.FileInfo) (device, inode uint64, ok bool) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, 0, false
	}
	return uint64(info.VolumeSerialNumber),
		uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), true
}
