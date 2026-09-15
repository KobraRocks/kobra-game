//go:build !windows

package sidecar

import (
	"golang.org/x/sys/unix"
)

// advisoryLock takes a non-blocking exclusive flock on the held lock file.
//
// The lock is advisory and is NOT the election mechanism — O_CREATE|O_EXCL is
// (§9.2). It exists so that a second process that somehow opens the same file
// is detectable by the kernel rather than only by our liveness check. Failure
// to take it is not fatal: the file content plus the health check remain
// authoritative.
func (l *Lock) advisoryLock() {
	if l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func (l *Lock) advisoryUnlock() {
	if l.f == nil {
		return
	}
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
}
