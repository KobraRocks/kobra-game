// Package update implements the launcher-assisted patch path of Launcher spec
// §19: release-manifest fetching, archive verification, tar.zst extraction, the
// crash-safe directory swap, and the §19.4 startup recovery state machine.
//
// This is the only package in the launcher that makes outbound network
// requests, and it does so only on the user's behalf (§3.3, FR-UPD-2). Every
// request uses a client with an explicit timeout, a bounded response body, and
// a small redirect cap.
//
// # Data protection (FR-UPD-7)
//
// Nothing in this package ever creates, moves or deletes anything under data/.
// An archive entry whose first path segment is data/ aborts verification and
// extraction with the update.data.rejected event; the caller surfaces E30.
//
// # Deferred work
//
// Signature verification of the manifest itself is NOT implemented: §19.3 step
// 2 and FR-LNCH-5 call for it, but the spec deliberately defers the choice of
// verifier and there is no vetted in-process verifier in the dependency set
// (§3.4). Verify therefore fails closed whenever a manifest declares a
// signature and no verifier has been installed with SetSignatureVerifier;
// silent acceptance is forbidden. Mod-archive extraction is likewise deferred
// (§27.5), as is the patch update channel (§27.7).
//
// # Error hygiene (§22.4)
//
// Every returned error is a *kobraerr.KobraError whose Msg and Detail contain
// no filesystem path, username or archive entry name. Paths and entry names
// travel in Cause only.
package update

import (
	"encoding/json"
	"os"
	"sync"

	"kobragames.local/launcher/internal/diagnostics"
)

// ReleaseManifestSchema is the exact value of a release manifest's "schema"
// field (release.manifest.schema.json).
const ReleaseManifestSchema = "kobra.release-manifest/1"

// UpdateStateSchema is the schema tag of the update.state marker document
// (§19.4). See swap.go for the document shape.
const UpdateStateSchema = "kobra.update-state/1"

// DataDirName is the one directory an update archive must never mention
// (FR-UPD-7).
const DataDirName = "data"

var (
	logMu sync.RWMutex
	log   *diagnostics.Logger
)

// SetLogger installs the structured logger used for the update.* events of
// Appendix C.5. It is optional: with no logger installed this package is
// silent. It is safe to call while an update is in flight, and passing nil
// removes the logger. The launcher installs one at startup; tests usually do
// not.
func SetLogger(l *diagnostics.Logger) {
	logMu.Lock()
	log = l
	logMu.Unlock()
}

func logAt(level diagnostics.Level, evt string, fields map[string]any) {
	logMu.RLock()
	l := log
	logMu.RUnlock()
	if l == nil {
		return
	}
	l.Event(level, evt, fields)
}

func logInfo(evt string, fields map[string]any)  { logAt(diagnostics.LevelInfo, evt, fields) }
func logWarn(evt string, fields map[string]any)  { logAt(diagnostics.LevelWarn, evt, fields) }
func logError(evt string, fields map[string]any) { logAt(diagnostics.LevelError, evt, fields) }

// fileExists reports whether path names an existing file or directory. A
// permission error is treated as "exists" so that recovery never mistakes an
// unreadable directory for a missing one.
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

// dirExists reports whether path exists and is a directory (following
// symlinks, which is what recovery cares about).
func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// InstalledRelease reads the release field from a release manifest at path. It
// returns "" with a nil error when the file is absent or declares no release,
// which is the documented "fall back to launcher.config.json" case (§19.2).
func InstalledRelease(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var doc struct {
		Release string `json:"release"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	return doc.Release, nil
}
