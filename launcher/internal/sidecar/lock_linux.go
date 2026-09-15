//go:build linux

package sidecar

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// This file implements the POSIX/Linux backstops of §9.3 using /proc, which
// requires no syscall and cannot signal another user's process.

// pidExists reports whether pid names a live process. On Linux, opening
// /proc/<pid>/stat is a pure read: it never delivers a signal, so it is safe
// when the pid belongs to another user.
func pidExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// processStartTime returns the process's start time derived from
// /proc/<pid>/stat field 22 (starttime, in clock ticks since boot) plus the
// system boot time. A zero time with a nil error means the platform did not
// expose it, and the caller then relies on the health check alone.
func processStartTime(pid int) (time.Time, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, err
	}
	s := string(raw)
	// The comm field is parenthesised and may contain spaces; skip past it.
	close := strings.LastIndex(s, ")")
	if close < 0 || close+1 >= len(s) {
		return time.Time{}, os.ErrInvalid
	}
	fields := strings.Fields(s[close+1:])
	// After the comm field, field 22 (starttime) is index 19 here.
	const starttimeIdx = 19
	if len(fields) <= starttimeIdx {
		return time.Time{}, os.ErrInvalid
	}
	ticks, err := strconv.ParseUint(fields[starttimeIdx], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, err
	}
	hz := uint64(100) // USER_HZ is 100 on Linux for every architecture Go supports.
	return boot.Add(time.Duration(ticks) * time.Second / time.Duration(hz)), nil
}

// bootTime reads the system boot time from /proc/stat's btime line.
func bootTime() (time.Time, error) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err != nil {
			return time.Time{}, err
		}
		return time.Unix(v, 0), nil
	}
	return time.Time{}, os.ErrNotExist
}
