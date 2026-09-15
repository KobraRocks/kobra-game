package update

// This file implements the recording half of the update trigger (Updater spec
// §6.1). POST /api/update/apply does not perform an update: it records intent
// durably and answers 202. The next launcher start performs the download,
// verification, extraction and swap before the server binds, which is what
// makes the swap invisible to any client and keeps it outside the serving
// window (decision D1, §7.3).

import (
	"context"
)

// IntentResult is the outcome of recording an update request.
type IntentResult struct {
	// Recorded is true when a pending marker is now on disk.
	Recorded bool
	// TargetRelease is the release the marker names.
	TargetRelease string
	// ArchiveSize is the compressed size the user is confirming (R6.4).
	ArchiveSize int64
	// NotesURL is the release-notes link, when latest.json declares one.
	NotesURL string
	// Detail is a short, path-free explanation for the response body.
	Detail string
}

// RecordUpdateIntent resolves the latest release, then writes the pending
// marker so that the next launch installs it (spec §6.1).
//
// It returns Recorded=false with a nil error when there is nothing to install,
// which the handler reports as "already current". An error means the check
// itself failed — the caller maps that onto the §19.2 fail-silently contract.
func RecordUpdateIntent(ctx context.Context, baseURL, installedRelease, sidecarDir, launcherVersion string) (IntentResult, error) {
	var out IntentResult

	latest, err := FetchLatest(ctx, baseURL)
	if err != nil {
		return out, err
	}
	out.NotesURL = latest.NotesURL
	if !ReleaseNewer(latest.Release, installedRelease) {
		out.Detail = "already current"
		return out, nil
	}

	// The manifest supplies the archive identity. It is fetched here, at
	// request time, so the size the user confirms is recorded in the marker and
	// re-validated at apply time (R6.2, R9.3).
	m, err := FetchManifest(ctx, latest.ManifestURL)
	if err != nil {
		return out, err
	}

	if _, err := EnsureUpdateDir(sidecarDir); err != nil {
		return out, err
	}

	p := &Pending{
		Schema:          PendingSchema,
		FromRelease:     installedRelease,
		TargetRelease:   m.Release,
		ManifestURL:     latest.ManifestURL,
		ArchiveURL:      m.ArchiveRef(),
		ArchiveSize:     m.declaredArchiveSize(),
		ArchiveHash:     m.declaredArchiveHash(),
		LauncherVersion: launcherVersion,
		Requested:       nowRFC3339(),
	}
	if err := WritePending(sidecarDir, p); err != nil {
		return out, err
	}

	out.Recorded = true
	out.TargetRelease = m.Release
	out.ArchiveSize = m.declaredArchiveSize()
	out.Detail = "The update will be applied the next time the launcher starts."
	logInfo("update.pending.recorded", map[string]any{
		"from":   installedRelease,
		"target": m.Release,
		"bytes":  m.declaredArchiveSize(),
	})
	return out, nil
}

// ArchiveRef returns the archive's published reference (its URL or name as
// written), before resolution against the manifest location.
func (m *Manifest) ArchiveRef() string {
	if m == nil || m.Archive == nil {
		return ""
	}
	if m.Archive.URL != "" {
		return m.Archive.URL
	}
	return m.Archive.Name
}

// ClearUpdateIntent removes the pending marker and any progress record, so a
// cancelled update leaves nothing for the next launch to act on (§12).
func ClearUpdateIntent(sidecarDir string) bool {
	removed := ClearPending(sidecarDir)
	ClearCancel(sidecarDir)
	if removed {
		logInfo("update.pending.cleared", map[string]any{"reason": "user_cancelled"})
	}
	return removed
}
