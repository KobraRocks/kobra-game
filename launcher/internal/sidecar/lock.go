package sidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LockInfo is the parsed contents of instance.lock (§9.1).
type LockInfo struct {
	Schema          string `json:"schema"`
	PID             int    `json:"pid"`
	StartTime       string `json:"start_time"`
	LauncherVersion string `json:"launcher_version"`
	GameID          string `json:"game_id"`
	Port            uint16 `json:"port"`
	// Instance is the opaque per-process id of §9.3's health check. The schema
	// in §9.1 does not list it, so it is additive and optional; readers must
	// tolerate its absence.
	Instance string `json:"instance,omitempty"`
}

// Lock is a held instance lock.
type Lock struct {
	dir      string
	path     string
	f        *os.File
	info     LockInfo
	reclaim  bool
	released bool
}

// LockState describes what was found on disk before acquisition.
type LockState int

const (
	// LockAbsent means no instance.lock file exists.
	LockAbsent LockState = iota
	// LockStale means a lock file exists but its owner is not live (§9.3).
	LockStale
	// LockLive means a lock file exists and its owner answered the health
	// check (§9.3).
	LockLive
)

// ReadLock parses an existing instance.lock. It returns (nil, LockAbsent, nil)
// when no lock file exists.
func ReadLock(dir string) (*LockInfo, LockState, error) {
	p := filepath.Join(dir, LockFileName)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, LockAbsent, nil
		}
		return nil, LockAbsent, err
	}
	var info LockInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		// A lock we cannot parse is stale by definition: §9.2 says a parse
		// failure means "treat as stale; reclaim".
		return nil, LockStale, fmt.Errorf("instance.lock is not valid JSON: %w", err)
	}
	return &info, LockStale, nil
}

// CheckLiveness implements §9.3. The health check over loopback is the primary
// signal; the PID check and the boot-time heuristic are backstops for the case
// where the port has been reused by a different process.
//
// dir is the sidecar directory holding instance.lock. It returns live=true only
// when the recorded instance answered /__kobra/health with a matching app,
// game_id and instance id.
func CheckLiveness(dir string, info *LockInfo, timeout time.Duration) (live bool, reason string) {
	if info == nil {
		return false, "no_lock"
	}
	if info.PID <= 0 {
		return false, "invalid_pid"
	}
	// Backstop 4: a lock older than this machine's boot time cannot belong to a
	// live process. This catches a lock left behind by a VM snapshot.
	if boot, err := bootTime(); err == nil && !boot.IsZero() {
		if mtime, err := lockMTime(dir); err == nil && mtime.Before(boot) {
			return false, "predates_boot"
		}
	}
	// Backstops 1 and 2: the PID must exist and, where the OS exposes it, its
	// start time must still match the recorded one (PID reuse).
	if !pidExists(info.PID) {
		return false, "pid_missing"
	}
	if st, err := processStartTime(info.PID); err == nil && !st.IsZero() {
		if recorded, err := time.Parse(time.RFC3339, info.StartTime); err == nil {
			if diff := st.Sub(recorded); diff > 2*time.Second || diff < -2*time.Second {
				return false, "pid_reused"
			}
		}
	}
	// Primary signal: §9.3 rule 3.
	if info.Port == 0 {
		return false, "no_port"
	}
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	h, err := fetchHealth(info.Port, timeout)
	if err != nil {
		return false, "health_unreachable"
	}
	if h.App != "kobra-launcher" || h.GameID != info.GameID {
		return false, "health_mismatch"
	}
	if info.Instance != "" && h.Instance != info.Instance {
		return false, "health_instance_mismatch"
	}
	return true, ""
}

// lockMTime is the lock file's modification time, used by the boot heuristic.
func lockMTime(dir string) (time.Time, error) {
	st, err := os.Stat(filepath.Join(dir, LockFileName))
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}

// AcquireLock creates instance.lock and takes the instance role (§9.2).
//
// When a lock file already exists but is not live, it is reclaimed (§9.6) and
// the returned Lock reports Reclaimed() == true. When a live lock exists,
// AcquireLock returns ErrLockHeld together with the existing LockInfo so the
// caller can decide between handoff and the two-folders prompt of §9.5.
func AcquireLock(dir, gameID, version string, port uint16, instance string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, LockFileName)

	if info, state, _ := ReadLock(dir); state != LockAbsent {
		if info != nil {
			if live, _ := CheckLiveness(dir, info, 500*time.Millisecond); live {
				return nil, &LockHeldError{Info: info}
			}
		}
		// Reclaim: remove and retry once (§9.6).
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return createLock(dir, path, gameID, version, port, instance, true)
	}
	return createLock(dir, path, gameID, version, port, instance, false)
}

func createLock(dir, path, gameID, version string, port uint16, instance string, reclaimed bool) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			// Another process won the race (§9.6).
			if info, _, _ := ReadLock(dir); info != nil {
				return nil, &LockHeldError{Info: info, Race: true}
			}
			return nil, ErrStartingRace
		}
		return nil, err
	}
	now := time.Now().UTC()
	info := LockInfo{
		Schema:          SchemaInstanceLock,
		PID:             os.Getpid(),
		StartTime:       now.Format(time.RFC3339),
		LauncherVersion: version,
		GameID:          gameID,
		Port:            port,
		Instance:        instance,
	}
	raw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	raw = append(raw, '\n')
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	l := &Lock{dir: dir, path: path, f: f, info: info, reclaim: reclaimed}
	l.advisoryLock()
	return l, nil
}

// LockHeldError reports that another launcher holds the instance role.
type LockHeldError struct {
	Info *LockInfo
	// Race is true when we lost a benign race during creation, which §9.6 says
	// means "Another launcher is starting. Try again in a moment."
	Race bool
}

func (e *LockHeldError) Error() string {
	if e.Info != nil {
		return fmt.Sprintf("instance lock held by pid %d on port %d", e.Info.PID, e.Info.Port)
	}
	return "instance lock held"
}

// ErrStartingRace is returned when the lock could not be reclaimed because
// another process created it between our read and our create.
var ErrStartingRace = errors.New("another launcher is starting")

// Info returns the identity recorded in the lock.
func (l *Lock) Info() LockInfo { return l.info }

// Port returns the port recorded in the lock.
func (l *Lock) Port() uint16 { return l.info.Port }

// Reclaimed reports whether this acquisition replaced a stale lock (§9.6).
func (l *Lock) Reclaimed() bool { return l.reclaim }

// Release closes and removes the lock. It is called in the DRAIN -> EXIT
// transition, after the socket is closed and the log is flushed (§9.7, §23.4).
func (l *Lock) Release() error {
	if l == nil || l.released {
		return nil
	}
	l.released = true
	l.advisoryUnlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// HeldLockPath returns the lock file path inside dir.
func HeldLockPath(dir string) string { return filepath.Join(dir, LockFileName) }

// Platform note: pidExists, processStartTime, bootTime and the advisory lock
// helpers are defined per-OS in lock_linux.go, lock_bsd.go (darwin and the
// BSDs) and lock_windows.go. Those are the platforms the launcher's build
// matrix covers; other GOOS values do not compile this package. There is no
// lock_other.go — an earlier comment here referred to one that never existed.

// SetPort rewrites the held lock file with the final bound port and instance
// id. The launcher binds the port after taking the lock (§4.2 orders lock
// before port), so the recorded identity is completed in place rather than by
// releasing and re-acquiring, which would open a handoff window.
func (l *Lock) SetPort(port uint16, instance string) error {
	if l == nil || l.released {
		return errors.New("lock has been released")
	}
	l.info.Port = port
	if instance != "" {
		l.info.Instance = instance
	}
	raw, err := json.MarshalIndent(l.info, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := l.f.WriteAt(raw, 0); err != nil {
		return err
	}
	// A shorter document must not leave trailing bytes; truncate to the file's
	// new length and flush.
	if err := l.f.Truncate(int64(len(raw))); err != nil {
		return err
	}
	return l.f.Sync()
}

// Instance returns the opaque instance id recorded in the lock.
func (l *Lock) Instance() string { return l.info.Instance }
