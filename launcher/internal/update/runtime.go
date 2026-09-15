package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// This file implements the machine-local substrate of the apply mechanism
// (Updater spec §5). Every artefact here lives under <sidecar>/update/ and is
// written atomically (spec R5.4): a temporary file in the same directory is
// written, fsynced and renamed into place, so a reader never observes a partial
// record. Nothing here may create, modify or delete anything under the game
// folder, and in particular nothing may touch data/ (FR-UPD-7).
//
// The names are deliberately explicit rather than clever. The update directory
// is the only place the launcher keeps an interrupted download, and a reader
// (a human, or the next launcher) must be able to tell at a glance what each
// file is.

// UpdateDirName is the per-game subdirectory of the sidecar that holds update
// state (§5).
const UpdateDirName = "update"

// The artefact names of §5. The archive is downloaded to DownloadFileName and
// verified in place, and Verify gates every read of it before the extractor
// touches it; the apply removes it afterwards. R5.1's rename is not performed
// (see the note in download.go).
const (
	PendingFileName  = "pending.json"
	DownloadFileName = "download.part"
	MetaFileName     = "download.meta.json"
	ProgressFileName = "progress.json"
	ResultFileName   = "result.json"
	CancelFileName   = "cancel"
	StartedFileName  = "started.json"
	ApplyLockName    = "apply.lock"
)

// Schema tags for the records this file owns.
const (
	PendingSchema  = "kobra.update-pending/1"
	ProgressSchema = "kobra.update-progress/1"
	ResultSchema   = "kobra.update-result/1"
	StartedSchema  = "kobra.update-started/1"
	MetaSchema     = "kobra.update-download/1"
)

// RetryBudget is the number of launches an update may be retried before the
// pending marker is abandoned (spec R6.6). A folder that can never download
// must not attempt a network fetch on every launch forever.
const RetryBudget = 3

// EnsureUpdateDir creates <sidecar>/update with the restrictive mode of spec
// R5.5 and returns its path.
func EnsureUpdateDir(sidecarDir string) (string, error) {
	if sidecarDir == "" {
		return "", errors.New("update: no sidecar directory")
	}
	dir := filepath.Join(sidecarDir, UpdateDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll honours umask; chmod makes the intent explicit for a directory
	// that may already exist from an older release.
	_ = os.Chmod(dir, 0o700)
	return dir, nil
}

// updatePath joins a named artefact onto <sidecar>/update.
func updatePath(sidecarDir, name string) string {
	return filepath.Join(sidecarDir, UpdateDirName, name)
}

// writeFileAtomic writes data to path via a sibling temporary file, fsyncing
// both the file and its directory (spec R5.4). The mode is fixed at 0600 (spec
// R5.5).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}

// writeJSONAtomic marshals v and writes it with writeFileAtomic.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return writeFileAtomic(path, b)
}

// readJSON reads path into v. A missing file returns ok=false and a nil error:
// every record in the update directory is optional, and "absent" is a normal
// state rather than a failure.
func readJSON(path string, v any) (ok bool, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, err
	}
	return true, nil
}

// syncDir fsyncs a directory so that a rename into it is durable. Failure is
// ignored: some filesystems do not permit it, and losing the durability of the
// directory entry is not a reason to fail an otherwise complete write.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// --- the pending marker (§6) ----------------------------------------------

// Pending is the recorded intent to install TargetRelease (spec R6.2). It is
// the only durable record that the user asked for an update, and it is written
// before the response that acknowledges the request, so a kill immediately
// after the 202 still leaves the update pending (spec R6.3).
//
// The URLs and the hash are recorded as observed at request time so that the
// size the user confirmed is the size that will be fetched. Apply re-validates
// them against a freshly fetched manifest (spec R9.3); a mismatch is E34, never
// a silent substitution.
type Pending struct {
	Schema          string `json:"schema"`
	FromRelease     string `json:"from_release"`
	TargetRelease   string `json:"target_release"`
	ManifestURL     string `json:"manifest_url"`
	ArchiveURL      string `json:"archive_url,omitempty"`
	ArchiveSize     int64  `json:"archive_size"`
	ArchiveHash     string `json:"archive_hash,omitempty"`
	LauncherVersion string `json:"launcher_version,omitempty"`
	Requested       string `json:"requested"`
	RetryCount      int    `json:"retry_count,omitempty"`
	LastAttempt     string `json:"last_attempt,omitempty"`
	LastReason      string `json:"last_reason,omitempty"`
}

// ReadPending loads the pending marker. Absent or unreadable yields nil, which
// callers treat as "no update requested".
func ReadPending(sidecarDir string) *Pending {
	var p Pending
	ok, err := readJSON(updatePath(sidecarDir, PendingFileName), &p)
	if !ok || err != nil {
		return nil
	}
	return &p
}

// WritePending records intent atomically. It also clears the previous cycle's
// progress record (spec R11.4) and the previous cycle's cancel flag (spec
// R12.5), so a stale "complete" or a stale cancel cannot affect the new update.
func WritePending(sidecarDir string, p *Pending) error {
	if p == nil {
		return errors.New("update: nil pending marker")
	}
	if _, err := EnsureUpdateDir(sidecarDir); err != nil {
		return err
	}
	if err := writeJSONAtomic(updatePath(sidecarDir, ProgressFileName), initialProgress(p)); err != nil {
		return err
	}
	removeIfPresent(updatePath(sidecarDir, CancelFileName))
	return writeJSONAtomic(updatePath(sidecarDir, PendingFileName), p)
}

// ClearPending removes the marker and reports whether it removed anything.
func ClearPending(sidecarDir string) bool {
	return removeIfPresent(updatePath(sidecarDir, PendingFileName))
}

// valid reports whether the marker is structurally usable.
func (p *Pending) valid() bool {
	if p == nil || p.Schema != PendingSchema {
		return false
	}
	if !validRelease(p.TargetRelease) {
		return false
	}
	if p.ManifestURL == "" {
		return false
	}
	return true
}

// resolveArchiveURL resolves the archive reference against the manifest
// document's own URL, so a relatively-published archive works. It returns ""
// when neither the marker nor the manifest names an archive.
func (p *Pending) resolveArchiveURL(m *Manifest) string {
	if p != nil && p.ArchiveURL != "" {
		if resolved, err := resolveURL(p.ManifestURL, p.ArchiveURL); err == nil {
			return resolved
		}
		return p.ArchiveURL
	}
	if m != nil && m.Archive != nil && m.Archive.URL != "" {
		if resolved, err := resolveURL(p.ManifestURL, m.Archive.URL); err == nil {
			return resolved
		}
		return m.Archive.URL
	}
	return ""
}

// --- the cancel flag (§12) ------------------------------------------------

// RequestCancel creates the cancel flag. Cancellation is a flag file because
// the download runs at startup with no browser session attached (spec R12.2);
// a file is the only channel that reaches a process which is not yet serving.
func RequestCancel(sidecarDir string) error {
	if _, err := EnsureUpdateDir(sidecarDir); err != nil {
		return err
	}
	return writeFileAtomic(updatePath(sidecarDir, CancelFileName), []byte("cancelled\n"))
}

// CancelRequested reports whether the cancel flag is present.
func CancelRequested(sidecarDir string) bool {
	return fileExists(updatePath(sidecarDir, CancelFileName))
}

// ClearCancel removes the cancel flag.
func ClearCancel(sidecarDir string) {
	removeIfPresent(updatePath(sidecarDir, CancelFileName))
}

// removeIfPresent removes path, reporting whether it existed.
func removeIfPresent(path string) bool {
	err := os.Remove(path)
	if err == nil {
		return true
	}
	if !os.IsNotExist(err) {
		// A directory where a file is expected, or a permission failure. Try
		// RemoveAll so a corrupt artefact cannot wedge every later launch.
		if rmErr := os.RemoveAll(path); rmErr == nil {
			return true
		}
	}
	return false
}

// --- disk space (§10) -----------------------------------------------------

// DiskSpace is the outcome of a free-space query for the volume holding a
// path. Known is false when the platform or filesystem cannot report free
// space, which must NOT refuse an update (spec R10.5).
type DiskSpace struct {
	Free  int64
	Known bool
}

// freeSpaceFn is the query the preflight uses. It is a variable so a test can
// drive the real preflight — including the R10.6 result record it writes —
// without needing a filesystem that is actually full. Production never
// reassigns it.
var freeSpaceFn = freeSpace

// RequiredBytes is the peak disk requirement for installing an update
// (spec R10.2):
//
//	archive.size + total_size + total_size + headroom
//
// The first total_size is the staging copy; the second is the backup copy,
// which coexists with the new release after the swap. headroom absorbs
// filesystem block slack, the launcher/ component and the download metadata.
func RequiredBytes(archiveSize, totalSize int64) int64 {
	if archiveSize < 0 {
		archiveSize = 0
	}
	if totalSize < 0 {
		totalSize = 0
	}
	base := archiveSize + totalSize + totalSize
	headroom := int64(64 << 20)
	if pct := totalSize / 20; pct > headroom { // 5% of total_size
		headroom = pct
	}
	return base + headroom
}

// nowRFC3339 is the single timestamp helper for every record in this file.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
