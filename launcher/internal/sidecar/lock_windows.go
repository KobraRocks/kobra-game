//go:build windows

package sidecar

import (
	"syscall"
	"time"
)

// pidExists on Windows opens the process with the minimal query right, which
// fails for a pid that does not exist.
func pidExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = syscall.CloseHandle(h)
	return true
}

// processStartTime is not exposed without a syscall wrapper; the health check
// is the primary signal (§9.3).
func processStartTime(pid int) (time.Time, error) {
	return time.Time{}, nil
}

// bootTime is not exposed portably; the boot heuristic is skipped.
func bootTime() (time.Time, error) {
	return time.Time{}, nil
}

// advisoryLock is a no-op on Windows: O_CREATE|O_EXCL remains the election
// primitive. The file is opened without FILE_SHARE_WRITE by default through
// os.OpenFile, which already prevents a second writer.
func (l *Lock) advisoryLock() {}

func (l *Lock) advisoryUnlock() {}
