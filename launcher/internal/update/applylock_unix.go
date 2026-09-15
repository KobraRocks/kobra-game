//go:build unix

package update

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// fileLock is an exclusive, non-blocking advisory lock on an open file
// (spec §17). It is an OS lock rather than a PID file so that a crashed apply
// releases it automatically when the process dies (spec R17.2).
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
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
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
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
