//go:build linux || darwin

package update

import (
	"golang.org/x/sys/unix"
)

// freeSpace reports the free bytes available on the filesystem holding path
// (spec §10). Known is false when the query fails, which the caller treats as
// "no opinion" rather than a refusal (spec R10.5).
//
// Bavail is used rather than Bfree: the launcher runs as an ordinary user, so
// the blocks reserved for root are not available to it.
func freeSpace(path string) DiskSpace {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return DiskSpace{}
	}
	bsize := int64(st.Bsize)
	if bsize <= 0 {
		return DiskSpace{}
	}
	free := int64(st.Bavail) * bsize
	if free < 0 {
		return DiskSpace{}
	}
	return DiskSpace{Free: free, Known: true}
}
