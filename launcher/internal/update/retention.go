package update

// This file implements rollback retention (Updater spec §15, FR-UPD-8).
//
// FR-UPD-8 requires game.old to be retained "until the new release has started
// successfully twice, then removed". Recover() preserves game.old
// unconditionally — correctly, because it cannot know the count — but nothing
// computed the count, so the retention window never closed and game.old
// occupied a full copy of the game forever.
//
// The count is incremented at the first heartbeat, not at startup: a release
// that starts and then dies before the shell connects must not count, because
// that is exactly the failure the two-start rule exists to catch (R15.3).

import (
	"os"
	"path/filepath"

	"kobragames.local/launcher/internal/paths"
)

// StartedRetireCount is how many successful starts retire the backup
// (FR-UPD-8, spec R15.5).
const StartedRetireCount = 2

// Started is the retention counter (spec R15.1).
type Started struct {
	Schema  string `json:"schema"`
	Release string `json:"release"`
	Count   int    `json:"count"`
	Updated string `json:"updated"`
}

// ReadStarted loads the counter, returning nil when there is none.
func ReadStarted(sidecarDir string) *Started {
	var s Started
	ok, err := readJSON(updatePath(sidecarDir, StartedFileName), &s)
	if !ok || err != nil {
		return nil
	}
	return &s
}

// BackupRelease reports the release the retained rollback copy holds, or "" when
// there is no usable backup. It is used for the user-facing revert offer
// (R15.11) and for keying the counter when the installed release cannot be
// determined.
//
// It reads game.old/ itself rather than update.state. The marker records the
// release being *installed* — not the backup — and it is deleted after every
// successful swap, so it can neither name the right release nor still be present
// at the moment the revert offer is needed. The full archive omits
// game/release.manifest.json, so the backup's own config is the fallback, the
// same order InstalledReleaseIn uses for the live tree.
func BackupRelease(gameFolder string) string {
	if !HasRetainedBackup(gameFolder) {
		return ""
	}
	backup := filepath.Join(gameFolder, backupDirName)
	if r, err := InstalledRelease(filepath.Join(backup, "release.manifest.json")); err == nil && r != "" {
		return r
	}
	return releaseFromConfig(filepath.Join(backup, filepath.FromSlash(configRelPath)))
}

// HasRetainedBackup reports whether a rollback copy exists beside a game
// directory, which is the state in which "Revert to previous version" must be
// offered (R15.11).
func HasRetainedBackup(gameFolder string) bool {
	return dirExists(filepath.Join(gameFolder, backupDirName)) && dirExists(filepath.Join(gameFolder, gameDirName))
}

// NoteSuccessfulStart records one successful start of the installed release and
// retires the backup once the release has started twice (spec R15.2-R15.6).
//
// It must be called from the heartbeat handler, once per process, and only
// after the server reached SERVE. installedRelease is the release the game
// folder reports; sidecarDir is the machine-local state root.
//
// A concurrent call is tolerated: the counter is a single small document
// rewritten atomically, so the worst outcome of a race is one increment short,
// never a corrupt record (R17.6).
func NoteSuccessfulStart(gameFolder, sidecarDir, installedRelease string) {
	if gameFolder == "" || sidecarDir == "" || installedRelease == "" {
		return
	}
	if _, err := EnsureUpdateDir(sidecarDir); err != nil {
		return
	}
	// R15.7: a folder that was never updated must not accumulate a count.
	if !HasRetainedBackup(gameFolder) {
		return
	}

	prev := ReadStarted(sidecarDir)
	count := 1
	// R15.4: a counter from an older update must not retire a newer backup.
	if prev != nil && prev.Release == installedRelease && prev.Count > 0 {
		count = prev.Count + 1
	}

	rec := Started{
		Schema:  StartedSchema,
		Release: installedRelease,
		Count:   count,
		Updated: nowRFC3339(),
	}
	if err := writeJSONAtomic(updatePath(sidecarDir, StartedFileName), rec); err != nil {
		logWarn("update.backup.retained", map[string]any{"release": installedRelease, "count": count, "reason": "record_failed"})
		return
	}

	if count < StartedRetireCount {
		logInfo("update.backup.retained", map[string]any{"release": installedRelease, "count": count})
		return
	}

	// R15.5: this is the only automatic deletion of game.old/.
	backup := filepath.Join(gameFolder, backupDirName)
	if err := os.RemoveAll(backup); err != nil {
		// R15.6: log once and leave the folder. Never retry in a loop.
		logWarn("update.backup.retained", map[string]any{"release": installedRelease, "count": count, "reason": "remove_failed"})
		return
	}
	removeIfPresent(updatePath(sidecarDir, StartedFileName))
	logInfo("update.backup.retired", map[string]any{"release": installedRelease})
}

// ClearStarted drops the retention counter. It is called after a rollback and
// after --repair (spec R15.9, R15.10): a rolled-back install has no backup to
// retire, and a stale count would retire the next update's backup prematurely.
func ClearStarted(sidecarDir string) {
	removeIfPresent(updatePath(sidecarDir, StartedFileName))
}

// --- installed release resolution (§6.2, §14.3) ---------------------------

// InstalledReleaseIn resolves the release the game folder actually reports.
//
// The order is the one Launcher spec §19.2 documents and Updater spec §14.3
// depends on:
//
//  1. game/release.manifest.json, when present. A hand-installed folder and a
//     game-only release have one;
//  2. launcher/launcher.config.json's release field. Under scope: full the
//     archive carries the config and deliberately omits the manifest
//     (Packaging spec §9.1), so this is the normal post-swap path;
//  3. "" when neither declares a release.
func InstalledReleaseIn(gameFolder string) string {
	manifest := filepath.Join(gameFolder, gameDirName, "release.manifest.json")
	if r, err := InstalledRelease(manifest); err == nil && r != "" {
		return r
	}
	cfgPath := filepath.Join(gameFolder, filepath.FromSlash(configRelPath))
	if r := releaseFromConfig(cfgPath); r != "" {
		return r
	}
	return ""
}

// releaseFromConfig reads the release field out of a launcher config document
// without validating it. A config that is invalid is a later startup failure
// with its own message; it must not stop an update from being reported.
func releaseFromConfig(path string) string {
	doc, ok, err := readConfigDoc(path)
	if !ok || err != nil {
		return ""
	}
	s, _ := doc["release"].(string)
	return s
}

// SidecarDirForGame resolves the machine-local state root for a game folder
// without requiring a validated config. It mirrors the launcher's own fallback
// order (paths.SetSidecar): the OS-native location when it is writable,
// otherwise the in-folder fallback.
//
// It exists for the pre-config startup pass, which needs the sidecar before
// launcher.config.json has been parsed. Unlike paths.SetSidecar it CREATES
// NOTHING: the native directory is created by initRootAndDiagnostics with the
// modes that step chooses, and creating the in-folder fallback here would
// litter a portable game folder with .kobra/ before the launcher has decided it
// needs one. A failure to resolve is reported as "" and simply disables the
// apply for this launch; the launcher then fails later with the real config
// error.
func SidecarDirForGame(gameFolder, gameID string) string {
	if gameID != "" {
		if native, err := paths.SidecarDirFor(gameID); err == nil && dirWritableExisting(native) {
			return native
		}
	}
	fallback := paths.FallbackSidecarDir(gameFolder)
	if dirWritableExisting(fallback) {
		return fallback
	}
	return ""
}

// dirWritableExisting reports whether dir already exists and accepts a probe
// file. It never creates dir.
func dirWritableExisting(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// dirWritable reports whether dir exists or can be created and accepts a probe
// file. It mirrors paths' unexported helper because the update directory may
// need to be created before paths.SetSidecar has run.
func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
