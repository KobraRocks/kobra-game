//go:build windows

package storage

import (
	"os"
	"syscall"
)

// On Windows the errno constants live in syscall. SyscallErrno is a distinct
// type that does not participate in errors.Is, so these predicates unwrap the
// *os.PathError chain and compare the raw errno, which is what a Windows
// filesystem error actually carries.
func syscallErrno(err error) (syscall.Errno, bool) {
	for err != nil {
		switch e := err.(type) {
		case *os.PathError:
			err = e.Err
		case *os.LinkError:
			err = e.Err
		case *os.SyscallError:
			err = e.Err
		case syscall.Errno:
			return e, true
		default:
			return 0, false
		}
	}
	return 0, false
}

func isEXDEV(err error) bool {
	e, ok := syscallErrno(err)
	return ok && e == syscall.EXDEV
}

// isEROFS reports a write-protected volume. ERROR_WRITE_PROTECT is 19; the
// constant is spelled numerically because syscall does not export it.
func isEROFS(err error) bool {
	const errorWriteProtect syscall.Errno = 19
	e, ok := syscallErrno(err)
	return ok && (e == errorWriteProtect || e == syscall.EROFS)
}

func isENOSPC(err error) bool {
	e, ok := syscallErrno(err)
	return ok && e == syscall.ENOSPC
}

// checkSpace is a no-op on Windows: GetDiskFreeSpaceEx is not wrapped here, so
// the guard is skipped and an ENOSPC surfaces from the write itself, which is
// the documented fallback (§17.5). This is a best-effort platform.
func (e *Engine) checkSpace(payloadBytes int64) error { return nil }
