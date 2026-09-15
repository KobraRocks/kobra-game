//go:build unix

package update

import (
	"errors"
	"syscall"
)

// isCrossDevice reports the EXDEV "invalid cross-device link" failure that a
// rename produces when its two paths are on different filesystems.
func isCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }
