package storage

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func tryLock(f *os.File) (bool, error) {
	var o windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &o)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func unlockFile(f *os.File) {
	// Closing a Windows handle releases locks asynchronously. Explicit unlock
	// lets another goroutine acquire the store immediately after this returns.
	var o windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &o)
}
