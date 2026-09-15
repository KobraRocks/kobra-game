//go:build windows

package update

import (
	"os"
	"syscall"
)

// isCrossDevice reports ERROR_NOT_SAME_DEVICE (EXDEV) on Windows. syscall.Errno
// does not participate in errors.Is on Windows, so the *os.LinkError chain is
// unwrapped and the raw errno compared, matching storage/fs_windows.go.
func isCrossDevice(err error) bool {
	for err != nil {
		switch e := err.(type) {
		case *os.PathError:
			err = e.Err
		case *os.LinkError:
			err = e.Err
		case *os.SyscallError:
			err = e.Err
		case syscall.Errno:
			return e == syscall.EXDEV
		default:
			return false
		}
	}
	return false
}
