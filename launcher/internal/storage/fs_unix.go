//go:build linux

package storage

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// The errno constants are taken from syscall, not x/sys/unix: syscall defines
// EXDEV, EROFS and ENOSPC on every platform Go supports, while the unix package
// does not.
func isEXDEV(err error) bool  { return errors.Is(err, syscall.EXDEV) }
func isEROFS(err error) bool  { return errors.Is(err, syscall.EROFS) }
func isENOSPC(err error) bool { return errors.Is(err, syscall.ENOSPC) }

// checkSpace implements the §17.5 disk-space guard on Linux: before a write,
// free space must exceed twice the request ceiling (room for the temp file and
// the rotation). On filesystems where free space is not queryable the check is
// skipped and the OS error surfaces instead.
func (e *Engine) checkSpace(payloadBytes int64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(e.dataDir, &st); err != nil {
		return nil // not queryable: skip, let the write report ENOSPC
	}
	need := e.maxRequest * 2
	if need <= 0 {
		need = 2 * payloadBytes
	}
	free := int64(st.Bavail) * int64(st.Bsize) //nolint:unconvert // Bsize is int64 on some arches
	if free < need {
		return errInsufficientSpace{}
	}
	return nil
}

// errInsufficientSpace is internal; mapIOError turns it into the documented
// 500 io_error with detail.reason = "insufficient_space".
type errInsufficientSpace struct{}

func (errInsufficientSpace) Error() string { return "insufficient space" }
