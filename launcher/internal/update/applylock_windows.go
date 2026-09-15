//go:build windows

package update

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// fileLock is an exclusive, non-blocking lock on an open file (spec §17). It is
// an OS lock rather than a PID file so that a crashed apply releases it
// automatically when the process dies (spec R17.2).
type fileLock struct {
	f *os.File
}

// errLockHeld reports that another apply already holds the lock.
var errLockHeld = errors.New("update: an update is already being applied")

// acquireFileLock takes the lock, or returns errLockHeld when it is held.
func acquireFileLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	ol := &windows.Overlapped{}
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errLockHeld
		}
		return nil, err
	}
	return &fileLock{f: f}, nil
}

// release drops the lock and closes the descriptor.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	ol := &windows.Overlapped{}
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, ol)
	_ = l.f.Close()
	l.f = nil
}
