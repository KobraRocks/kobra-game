//go:build !linux && !windows

package storage

import (
	"errors"
	"syscall"
)

// The errno predicates are identical across POSIX platforms; only the
// disk-space guard is Linux-specific (see fs_unix.go), because Statfs_t has a
// different shape on Darwin and the BSDs and the launcher is Linux-first.
func isEXDEV(err error) bool  { return errors.Is(err, syscall.EXDEV) }
func isEROFS(err error) bool  { return errors.Is(err, syscall.EROFS) }
func isENOSPC(err error) bool { return errors.Is(err, syscall.ENOSPC) }

// checkSpace is a no-op where free space is not queryable through this build,
// which is the §17.5 documented fallback: the OS error surfaces from the write
// itself.
func (e *Engine) checkSpace(payloadBytes int64) error { return nil }
