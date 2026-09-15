//go:build !linux && !darwin && !windows

package update

// freeSpace reports "no opinion" on the platforms without a wrapped statfs:
// solaris, illumos, the BSDs and plan9. Known is false, and R10.5 says an
// unqueryable free-space figure must not refuse an update, so the install
// proceeds and a genuine ENOSPC surfaces from the write itself.
//
// Without this file the package did not compile there at all: space_unix.go was
// tagged "unix" while reading Statfs_t fields that only Linux and Darwin
// spell that way.
func freeSpace(path string) DiskSpace { return DiskSpace{} }
