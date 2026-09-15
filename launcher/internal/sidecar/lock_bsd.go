//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package sidecar

import (
	"syscall"
	"time"
)

// pidExists on the BSDs and macOS uses signal 0, which performs permission and
// existence checking without delivering a signal.
//
// Deviation from §9.3: processStartTime is not available without a syscall
// wrapper on these platforms, so PID-reuse detection relies on the health
// check that follows it. This is a best-effort platform (the project is
// Linux-first), and the health check is documented as the primary signal.
func pidExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// processStartTime is deliberately unimplemented here; the caller treats a nil
// error with a zero time as "not exposed" and falls through to the health
// check.
func processStartTime(pid int) (time.Time, error) {
	return time.Time{}, nil
}

// bootTime is not exposed portably; the boot heuristic is skipped.
func bootTime() (time.Time, error) {
	return time.Time{}, nil
}
