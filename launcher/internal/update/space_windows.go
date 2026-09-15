//go:build windows

package update

import (
	"golang.org/x/sys/windows"
)

// freeSpace reports the free bytes available on the volume holding path
// (spec §10). Known is false when the query fails, which the caller treats as
// "no opinion" rather than a refusal (spec R10.5).
func freeSpace(path string) DiskSpace {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return DiskSpace{}
	}
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeToCaller, &total, &totalFree); err != nil {
		return DiskSpace{}
	}
	if freeToCaller > 1<<62 {
		return DiskSpace{}
	}
	return DiskSpace{Free: int64(freeToCaller), Known: true}
}
