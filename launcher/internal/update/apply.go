package update

// This file implements Apply (Updater spec §9): the ordered pipeline that is the
// only caller of GuardApply, DownloadArchive, Verify, Extract, Swap and the
// config merge. One read of this file tells the whole story of an update, which
// is the auditability the spec asks for (R9.1).
//
// Apply runs at startup, after Recover and before the strict layout check, and
// therefore before the instance lock, the port, or any socket exists (spec
// §7.1-§7.3). No client can observe a half-swapped game folder.
//
// It never returns an error that should stop the launcher: a failed update
// leaves the current release runnable and is reported as an ApplyResult
// (spec R7.9).

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kobragames.local/launcher/internal/kobraerr"
)

// ApplyOutcome is the machine-readable ending of one apply.
type ApplyOutcome string

const (
	// ApplyNoPending: there was nothing to do.
	ApplyNoPending ApplyOutcome = "no_pending"
	// ApplyApplied: the swap completed.
	ApplyApplied ApplyOutcome = "applied"
	// ApplyAlreadyCurrent: the installed release already equals the target.
	ApplyAlreadyCurrent ApplyOutcome = "already_current"
	// ApplySkipped: the update was not attempted (locked, fast path, abort).
	ApplySkipped ApplyOutcome = "skipped"
	// ApplyCancelled: the user cancelled (E34).
	ApplyCancelled ApplyOutcome = "cancelled"
	// ApplyFailed: the update did not happen and the marker may be retained.
	ApplyFailed ApplyOutcome = "failed"
)

// ApplyOptions tunes one apply. The zero value is the production behaviour.
type ApplyOptions struct {
	// LauncherVersion is the running launcher's version, evaluated against
	// launcher_min at apply time rather than at request time (R9.2).
	LauncherVersion string
	// MaxArchiveBytes is the policy ceiling on the declared archive size.
	MaxArchiveBytes int64
	// StallTimeout overrides the download idle deadline (tests). Zero, which
	// is what production passes, means dlStallTimeout.
	StallTimeout time.Duration
	// SkipDownloadCheckForTest disables the disk preflight (tests only).
	SkipDownloadCheckForTest bool
}

// ApplyResult reports what Apply did, for the startup log and the record.
type ApplyResult struct {
	Outcome          ApplyOutcome
	FromRelease      string
	TargetRelease    string
	InstalledRelease string
	Reason           string
	Message          string
	BytesWritten     int64
	RetainedBackup   bool
	// Retryable is true when the pending marker was kept for another attempt.
	Retryable bool
}

// Apply performs at most one pending update against gameFolder (spec §9.1).
//
// sidecarDir is the machine-local state root; launcherVersion gates
// launcher_min. A nil or unrecognised marker is a no-op. An apply that cannot
// proceed leaves the folder exactly as it found it, and the returned error is
// always nil: every failure is reported through ApplyResult so the launcher can
// carry on with the installed release (spec R7.9).
func Apply(ctx context.Context, gameFolder, sidecarDir string, opts ApplyOptions) ApplyResult {
	var res ApplyResult
	if strings.TrimSpace(gameFolder) == "" || strings.TrimSpace(sidecarDir) == "" {
		res.Outcome = ApplySkipped
		res.Reason = "no_folder"
		return res
	}

	// §17: at most one apply per game folder. The lock is an OS lock, so a
	// crashed apply releases it when the process dies (R17.2).
	dir, err := EnsureUpdateDir(sidecarDir)
	if err != nil {
		res.Outcome = ApplySkipped
		res.Reason = "sidecar_unwritable"
		return res
	}
	lock, err := acquireFileLock(filepath.Join(dir, ApplyLockName))
	if err != nil {
		if errors.Is(err, errLockHeld) {
			res.Outcome = ApplySkipped
			res.Reason = "locked"
			logInfo("update.apply.skipped", map[string]any{"reason": "locked"})
			return res
		}
		res.Outcome = ApplySkipped
		res.Reason = "lock_failed"
		logWarn("update.apply.skipped", map[string]any{"reason": "lock_failed"})
		return res
	}
	defer lock.release()

	p := ReadPending(sidecarDir)
	if p == nil {
		res.Outcome = ApplyNoPending
		return res
	}
	res.FromRelease = p.FromRelease
	res.TargetRelease = p.TargetRelease

	// Step 1: validate the marker.
	if !p.valid() {
		ClearPending(sidecarDir)
		logInfo("update.pending.cleared", map[string]any{"reason": "invalid_marker"})
		res.Outcome = ApplyFailed
		res.Reason = "invalid_marker"
		res.Message = kobraerr.From(configDamaged("invalid_marker", nil)).Msg
		return res
	}
	// A marker that does not move forward is a bug or a downgrade attempt.
	if p.FromRelease != "" && !ReleaseNewer(p.TargetRelease, p.FromRelease) {
		ClearPending(sidecarDir)
		logInfo("update.pending.cleared", map[string]any{"reason": "not_newer"})
		res.Outcome = ApplyFailed
		res.Reason = "not_newer"
		res.Message = "The update is not newer than the installed release."
		return res
	}

	// R12: a cancellation recorded before this launch is honoured before any
	// other work. POST /api/update/cancel writes this flag AND clears the
	// marker, so reaching here with the flag set means the clear did not happen
	// (a crash, or a direct write) — either way the user's intent is to stop,
	// and no network request should be made to discover that.
	if CancelRequested(sidecarDir) {
		ClearPending(sidecarDir)
		ClearCancel(sidecarDir)
		logInfo("update.cancel.honoured", map[string]any{"phase": PhaseIdle})
		res.Outcome = ApplyCancelled
		res.Reason = "cancelled"
		res.Message = msgCancelled
		progress := newProgressWriter(sidecarDir, p)
		progress.setPhase(PhaseCancelled, res.Message)
		WriteResult(sidecarDir, Result{
			Outcome:       OutcomeCancelled,
			TargetRelease: p.TargetRelease,
			Reason:        "cancelled",
			Message:       res.Message,
		})
		return res
	}

	// R6.5/R6.6: the retry budget is checked before any network work.
	if p.RetryCount >= RetryBudget {
		ClearPending(sidecarDir)
		WriteResult(sidecarDir, Result{
			Outcome:       OutcomeFailed,
			TargetRelease: p.TargetRelease,
			Reason:        "retries_exhausted",
			Message:       "The update could not be downloaded. The launcher may be offline.",
		})
		logWarn("update.apply.failed", map[string]any{"reason": "retries_exhausted"})
		res.Outcome = ApplyFailed
		res.Reason = "retries_exhausted"
		res.Message = "The update could not be downloaded. The launcher may be offline."
		return res
	}

	// Step 2: what is installed right now.
	installed := InstalledReleaseIn(gameFolder)
	res.InstalledRelease = installed

	// Step 3: already applied? This is reachable when a crash landed between
	// the swap and the marker deletion.
	if installed != "" && installed == p.TargetRelease {
		ClearPending(sidecarDir)
		logInfo("update.apply.skipped", map[string]any{"reason": "already_current"})
		res.Outcome = ApplyAlreadyCurrent
		res.Reason = "already_current"
		res.Message = "The update was already installed."
		WriteResult(sidecarDir, Result{
			Outcome:          OutcomeSkipped,
			TargetRelease:    p.TargetRelease,
			InstalledRelease: installed,
			Reason:           "already_current",
			Message:          res.Message,
		})
		return res
	}

	progress := newProgressWriter(sidecarDir, p)
	progress.setPhase(PhaseVerify, "Starting the update.")

	// Step 4: fetch the manifest.
	m, err := FetchManifest(ctx, p.ManifestURL)
	if err != nil {
		return failRetryable(sidecarDir, progress, p, res, "manifest_unreachable", msgDownloadFailed, err)
	}

	// Step 5: reconcile the marker with the manifest (R9.3).
	if err := reconcilePending(p, m); err != nil {
		ClearPending(sidecarDir)
		res.Outcome = ApplyFailed
		res.Reason = "manifest_drift"
		res.Message = msgDriftMismatch
		progress.setPhase(PhaseFailed, res.Message)
		logWarn("update.manifest.drift", map[string]any{"field": err.Error()})
		WriteResult(sidecarDir, Result{
			Outcome:       OutcomeFailed,
			TargetRelease: p.TargetRelease,
			Reason:        "manifest_drift",
			Message:       res.Message,
		})
		return res
	}

	// Step 6: launcher_min, evaluated against the running launcher (R9.2).
	if err := GuardApply(m, opts.LauncherVersion); err != nil {
		ClearPending(sidecarDir)
		res.Outcome = ApplyFailed
		res.Reason = "launcher_min"
		res.Message = LauncherTooOldMessage
		progress.setPhase(PhaseFailed, res.Message)
		WriteResult(sidecarDir, Result{
			Outcome:       OutcomeFailed,
			TargetRelease: p.TargetRelease,
			Reason:        "launcher_min",
			Message:       res.Message,
		})
		return res
	}

	archiveSize := m.declaredArchiveSize()
	archiveURL := p.resolveArchiveURL(m)

	// Step 7: disk-space preflight (spec §10).
	if !opts.SkipDownloadCheckForTest {
		required := RequiredBytes(archiveSize, m.TotalSize)
		space := freeSpaceFn(spacePath(gameFolder))
		switch {
		case !space.Known:
			logInfo("update.disk.unknown", map[string]any{"reason": "query_failed"})
		case space.Free < required:
			// R6.5: retain the marker; the user may free space. R10.6: the
			// result record carries the byte counts so the UI can explain the
			// refusal instead of only naming it.
			progress.setPhase(PhaseFailed, msgInsufficientSpace)
			WriteResult(sidecarDir, Result{
				Outcome:        OutcomeFailed,
				TargetRelease:  p.TargetRelease,
				Reason:         "insufficient_space",
				Message:        msgInsufficientSpace,
				RequiredBytes:  required,
				AvailableBytes: space.Free,
			})
			logWarn("update.disk.insufficient", map[string]any{
				"required":  required,
				"available": space.Free,
			})
			res.Outcome = ApplyFailed
			res.Reason = "insufficient_space"
			res.Message = msgInsufficientSpace
			res.Retryable = true
			return res
		}
	}

	// Step 8: download. The declared size is authoritative (R8.2, R8.19).
	if archiveSize <= 0 {
		ClearPending(sidecarDir)
		res.Outcome = ApplyFailed
		res.Reason = "no_archive_size"
		res.Message = msgCannotDownload
		progress.setPhase(PhaseFailed, res.Message)
		WriteResult(sidecarDir, Result{
			Outcome:       OutcomeFailed,
			TargetRelease: p.TargetRelease,
			Reason:        "no_archive_size",
			Message:       res.Message,
		})
		return res
	}
	if archiveURL == "" {
		ClearPending(sidecarDir)
		res.Outcome = ApplyFailed
		res.Reason = "no_archive_url"
		res.Message = msgCannotDownload
		progress.setPhase(PhaseFailed, res.Message)
		WriteResult(sidecarDir, Result{
			Outcome:       OutcomeFailed,
			TargetRelease: p.TargetRelease,
			Reason:        "no_archive_url",
			Message:       res.Message,
		})
		return res
	}

	progress.setPhase(PhaseDownload, "Downloading the update.")
	logInfo("update.download.begin", map[string]any{"bytes": archiveSize})

	dl, dlErr := DownloadArchive(ctx, archiveURL, updatePath(sidecarDir, DownloadFileName), archiveSize, DownloadOptions{
		MaxArchiveBytes: opts.MaxArchiveBytes,
		ExpectedHash:    p.ArchiveHash,
		StallTimeout:    opts.StallTimeout,
		Progress: func(written, total int64) {
			progress.setBytes(written, total)
		},
		ShouldCancel: func() bool { return CancelRequested(sidecarDir) },
	})
	res.BytesWritten = dl.Bytes
	if dlErr != nil {
		if IsCancelled(dlErr) {
			res.Outcome = ApplyCancelled
			res.Reason = "cancelled"
			res.Message = "The update was cancelled."
			progress.setPhase(PhaseCancelled, res.Message)
			WriteResult(sidecarDir, Result{
				Outcome:       OutcomeCancelled,
				TargetRelease: p.TargetRelease,
				Reason:        "cancelled",
				Message:       res.Message,
			})
			ClearPending(sidecarDir)
			ClearCancel(sidecarDir)
			return res
		}
		if errors.Is(dlErr, errInsecureScheme) || errors.Is(dlErr, errArchiveTooLarge) ||
			errors.Is(dlErr, errNoArchiveSize) {
			ClearPending(sidecarDir)
			res.Outcome = ApplyFailed
			res.Reason = reasonForDownloadPolicy(dlErr)
			res.Message = msgCannotDownload
			progress.setPhase(PhaseFailed, res.Message)
			WriteResult(sidecarDir, Result{
				Outcome:       OutcomeFailed,
				TargetRelease: p.TargetRelease,
				Reason:        res.Reason,
				Message:       res.Message,
			})
			logWarn("update.download.failed", map[string]any{"reason": res.Reason})
			return res
		}
		return failRetryable(sidecarDir, progress, p, res, "download_failed", msgDownloadFailed, dlErr)
	}
	logInfo("update.download.done", map[string]any{"bytes": dl.Bytes})

	// The archive path survives until extraction has read it.
	archivePath := dl.Path

	// Step 9: verify before anything is written.
	progress.setPhase(PhaseVerify, "Checking the update.")
	if err := Verify(archivePath, m); err != nil {
		ClearPending(sidecarDir)
		removeIfPresent(archivePath)
		removeIfPresent(updatePath(sidecarDir, MetaFileName))
		return failTerminal(sidecarDir, progress, p, res, "verify_failed", msgForDataViolation(err))
	}

	// Step 10: extract into the staging root. A previous crashed or aborted
	// attempt may have left game.new/ or launcher.new/ behind; RemoveAll is how
	// this stage disposes of that debris (Recover also cleans it at startup).
	stagingRoot := StagingRootFor(sidecarDir)
	if stagingRoot == "" {
		return failRetryable(sidecarDir, progress, p, res, "no_staging_root", msgStagingUnavailable, nil)
	}
	for _, name := range []string{stagingDirName, launcherStagingDirName} {
		if d := filepath.Join(stagingRoot, name); dirExists(d) {
			if err := os.RemoveAll(d); err != nil {
				return failRetryable(sidecarDir, progress, p, res, "staging_unwritable", msgStagingUnavailable, err)
			}
		}
	}
	before, snapErr := snapshotDataDir(filepath.Join(gameFolder, DataDirName))
	if snapErr != nil {
		// A data/ we cannot read is not a reason to refuse an update; the
		// post-extraction assertion simply cannot run. Extract still enforces
		// FR-UPD-7 by construction.
		logWarn("update.data.snapshot_failed", map[string]any{"reason": "unreadable"})
	}
	progress.setPhase(PhaseExtract, "Installing the update.")
	logInfo("update.extract.begin", map[string]any{"release": m.Release})
	if err := Extract(archivePath, stagingRoot, m); err != nil {
		_ = os.RemoveAll(filepath.Join(stagingRoot, stagingDirName))
		_ = os.RemoveAll(filepath.Join(stagingRoot, launcherStagingDirName))
		removeIfPresent(archivePath)
		removeIfPresent(updatePath(sidecarDir, MetaFileName))
		ClearPending(sidecarDir)
		_ = err
		return failTerminal(sidecarDir, progress, p, res, "extract_failed", msgForDataViolation(err))
	}
	logInfo("update.extract.done", map[string]any{"release": m.Release})

	// R5.2: the archive has been read, so it is no longer needed, and removing
	// it here keeps the disk-space peak down for the swap that follows. This
	// must happen AFTER extraction, never before.
	removeIfPresent(archivePath)
	removeIfPresent(updatePath(sidecarDir, MetaFileName))

	// Step 11: the independent data/ assertion (R9.5).
	if snapErr == nil {
		if err := assertDataUnchanged(filepath.Join(gameFolder, DataDirName), before); err != nil {
			_ = os.RemoveAll(filepath.Join(stagingRoot, stagingDirName))
			_ = os.RemoveAll(filepath.Join(stagingRoot, launcherStagingDirName))
			ClearPending(sidecarDir)
			logError("update.data.rejected", map[string]any{"release": m.Release})
			out := failTerminal(sidecarDir, progress, p, res, "data_touched", msgData)
			return out
		}
	}

	// Step 12: merge the release's config with the local one (spec §14). This
	// runs against the staged launcher tree, before it is promoted, so an
	// invalid result aborts the update rather than installing it (R14.5).
	if _, err := MergeConfigInto(gameFolder, stagingRoot); err != nil {
		_ = os.RemoveAll(filepath.Join(stagingRoot, stagingDirName))
		_ = os.RemoveAll(filepath.Join(stagingRoot, launcherStagingDirName))
		ClearPending(sidecarDir)
		return failTerminal(sidecarDir, progress, p, res, "config_merge_failed", msgDamagedShort)
	}

	// Step 13: the swap. R12.7 makes this the last point at which a
	// cancellation can be honoured: there is no safe stop between "verified and
	// staged" and "swapped", because stopping would leave a game.new/ that a
	// later launch would silently promote — a state change the user did not
	// confirm. A cancel requested before this point is honoured here, with the
	// install still untouched.
	if CancelRequested(sidecarDir) {
		_ = os.RemoveAll(filepath.Join(stagingRoot, stagingDirName))
		_ = os.RemoveAll(filepath.Join(stagingRoot, launcherStagingDirName))
		removeIfPresent(archivePath)
		ClearPending(sidecarDir)
		ClearCancel(sidecarDir)
		logInfo("update.cancel.honoured", map[string]any{"phase": PhaseSwap})
		res.Outcome = ApplyCancelled
		res.Reason = "cancelled"
		res.Message = msgCancelled
		progress.setPhase(PhaseCancelled, res.Message)
		WriteResult(sidecarDir, Result{
			Outcome:       OutcomeCancelled,
			TargetRelease: p.TargetRelease,
			Reason:        "cancelled",
			Message:       res.Message,
		})
		return res
	}

	progress.setPhase(PhaseSwap, "Finishing the update.")
	logInfo("update.swap.begin", map[string]any{"release": p.TargetRelease})
	if err := Swap(gameFolder, stagingRoot, p.TargetRelease); err != nil {
		// The live folder has not been touched yet, so it still reports the
		// release it actually holds and the update stays retryable. Swap also
		// restores game.old for failures it can recover from; if it could not,
		// update.state is left in place for Recover at the next start.
		return failTerminal(sidecarDir, progress, p, res, "swap_failed", msgSwapFailed)
	}
	logInfo("update.swap.complete", map[string]any{"release": p.TargetRelease})

	// Step 13b: replace launcher/ in place (Packaging spec §9.3) so the running
	// binary survives its own release.
	//
	// This runs AFTER the swap, deliberately. launcher.config.json carries the
	// release-owned `release` key, and when the archive omits
	// game/release.manifest.json — the normal full-archive shape — that config is
	// what InstalledReleaseIn reports. Replacing it before the swap meant a
	// failed swap left the folder claiming a release it had never installed, so
	// the retry short-circuited as already_current and the update was silently
	// lost.
	if err := MergeLauncherInto(stagingRoot, gameFolder); err != nil {
		// The game tree is installed but launcher/ was not replaced. Keep the
		// marker so the next start retries the merge within the retry budget
		// instead of reporting the folder as up to date.
		_ = os.RemoveAll(filepath.Join(stagingRoot, launcherStagingDirName))
		return failRetryable(sidecarDir, progress, p, res, "launcher_merge_failed", msgLauncherMerge, err)
	}
	// The staged launcher tree has been merged; drop it so a later launch does
	// not re-merge it over a newer one.
	_ = os.RemoveAll(filepath.Join(stagingRoot, launcherStagingDirName))

	// Step 14: record the outcome and clear the intent.
	installedAfter := InstalledReleaseIn(gameFolder)
	res.Outcome = ApplyApplied
	res.InstalledRelease = installedAfter
	res.RetainedBackup = HasRetainedBackup(gameFolder)
	res.Message = "The update was installed."
	progress.setPhase(PhaseComplete, res.Message)
	WriteResult(sidecarDir, Result{
		Outcome:          OutcomeComplete,
		TargetRelease:    p.TargetRelease,
		InstalledRelease: installedAfter,
		Reason:           "applied",
		Message:          res.Message,
		BytesWritten:     res.BytesWritten,
		RetainedBackup:   res.RetainedBackup,
	})
	ClearPending(sidecarDir)
	ClearCancel(sidecarDir)
	logInfo("update.apply.done", map[string]any{"target": p.TargetRelease, "installed": installedAfter})
	if res.RetainedBackup {
		// The counter starts at the first successful start, not here (R15.3).
		logInfo("update.backup.retained", map[string]any{"release": installedAfter, "count": 0})
	}
	return res
}

// --- error helpers --------------------------------------------------------

// Error wordings for the E-codes of Updater spec §16. The first four are the
// FS §16 texts verbatim, which Packaging spec §9.4 requirement 5 requires the
// update UI to show word for word; msgDamaged (verify.go) carries the longer
// re-download advice that only the Verify path uses.
const (
	msgInsufficientSpace = "There isn't enough free space to install this update."
	msgCannotDownload    = "This update cannot be downloaded."
	msgCancelled         = "The update was cancelled."
	msgDriftMismatch     = "The update changed since you confirmed it. Check for updates again."
	msgDownloadFailed    = "The update could not be downloaded. The launcher may be offline."
	msgDamagedShort      = "The update file is damaged."
	// msgStagingUnavailable covers the staging-root failures: the update was
	// never installed, so the wording must not claim it was downloaded.
	msgStagingUnavailable = "The update could not be prepared for installation."
	// msgLauncherMerge is the post-swap launcher failure: the release is
	// installed but launcher/ was not replaced, so the marker is kept and the
	// next start retries.
	msgLauncherMerge = "The update was installed, but the launcher could not be updated. It will be retried on the next start."
)

// failRetryable keeps the marker for one more attempt (R6.5, R6.6) and records
// the failure with the message that matches the reason. It returns the populated
// result.
//
// cause is recorded only as its presence: every update.* event is path-free, and
// the diagnostics payload exposes the log tail to the shell (§22.4).
func failRetryable(sidecarDir string, progress *progressWriter, p *Pending, res ApplyResult, reason, message string, cause error) ApplyResult {
	p.RetryCount++
	p.LastAttempt = nowRFC3339()
	p.LastReason = reason
	if err := writeJSONAtomic(updatePath(sidecarDir, PendingFileName), p); err != nil {
		logWarn("update.apply.failed", map[string]any{"reason": "marker_rewrite"})
	}
	exhausted := p.RetryCount >= RetryBudget
	if exhausted {
		ClearPending(sidecarDir)
	}
	if progress != nil {
		progress.setPhase(PhaseFailed, message)
	}
	WriteResult(sidecarDir, Result{
		Outcome:       OutcomeFailed,
		TargetRelease: p.TargetRelease,
		Reason:        reason,
		Message:       message,
	})
	logWarn("update.apply.failed", map[string]any{
		"reason":        reason,
		"cause_present": cause != nil,
		"retryable":     !exhausted,
	})
	res.Outcome = ApplyFailed
	res.Reason = reason
	res.Message = message
	res.Retryable = !exhausted
	return res
}

// failTerminal clears the marker and records a non-retryable failure. The
// returned result is fully populated.
func failTerminal(sidecarDir string, progress *progressWriter, p *Pending, res ApplyResult, reason, message string) ApplyResult {
	ClearPending(sidecarDir)
	if progress != nil {
		progress.setPhase(PhaseFailed, message)
	}
	WriteResult(sidecarDir, Result{
		Outcome:       OutcomeFailed,
		TargetRelease: p.TargetRelease,
		Reason:        reason,
		Message:       message,
	})
	logWarn("update.apply.failed", map[string]any{"reason": reason})
	res.Outcome = ApplyFailed
	res.Reason = reason
	res.Message = message
	return res
}

// reasonForDownloadPolicy names a refusal that no retry will fix.
func reasonForDownloadPolicy(err error) string {
	switch {
	case errors.Is(err, errInsecureScheme):
		return "insecure_scheme"
	case errors.Is(err, errArchiveTooLarge):
		return "archive_too_large"
	case errors.Is(err, errNoArchiveSize):
		return "no_archive_size"
	default:
		return "download_refused"
	}
}

// msgForDataViolation maps a rejection onto the E30 wording when the archive
// touched data/, and E28 otherwise. Both Verify and Extract reject a data/
// member (verify.go refuses to hash it, extract.go refuses to write it), and
// both must report the same user-facing sentence: the E30 text is the one that
// tells the user their saves were protected rather than that the file is
// corrupt, and FR-UPD-7 is the product's central promise.
func msgForDataViolation(err error) string {
	var ke *kobraerr.KobraError
	if errors.As(err, &ke) {
		if ke.Msg == msgData {
			return msgData
		}
	}
	return msgDamagedShort
}

// --- reconciliation (§9.2 step 5) -----------------------------------------

// reconcilePending compares the confirmed marker against the freshly fetched
// manifest (R9.3). It returns the offending field name on a mismatch, which is
// safe to log and to publish: it is a field name, not a value.
func reconcilePending(p *Pending, m *Manifest) error {
	if m == nil {
		return errors.New("manifest")
	}
	if p.TargetRelease != m.Release {
		return errors.New("release")
	}
	if p.ArchiveSize > 0 && m.declaredArchiveSize() != p.ArchiveSize {
		return errors.New("archive_size")
	}
	if p.ArchiveHash != "" && m.declaredArchiveHash() != p.ArchiveHash {
		return errors.New("archive_hash")
	}
	// A marker that named a specific archive must still be the archive the
	// manifest now names. A publisher who swapped the payload for the same
	// release is exactly the substitution this check exists to catch.
	if p.ArchiveURL != "" {
		want, err := resolveURL(p.ManifestURL, p.ArchiveURL)
		if err != nil {
			want = p.ArchiveURL
		}
		got := p.resolveArchiveURL(m)
		if got != "" && want != got {
			return errors.New("archive_url")
		}
	}
	return nil
}

// --- the data/ assertion (§9.5) -------------------------------------------

// dataSnapshot is the entry list and modification times of data/ before an
// update touches the disk.
type dataSnapshot struct {
	entries map[string]int64
}

// snapshotDataDir records data/'s shape. A missing data/ yields an empty
// snapshot rather than an error: a fresh install may legitimately have none.
func snapshotDataDir(dataDir string) (dataSnapshot, error) {
	snap := dataSnapshot{entries: map[string]int64{}}
	if !dirExists(dataDir) {
		return snap, nil
	}
	err := filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(dataDir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		snap.entries[rel] = info.ModTime().UnixNano()
		return nil
	})
	if err != nil {
		return snap, err
	}
	return snap, nil
}

// assertDataUnchanged re-enforces FR-UPD-7 independently of Extract (R9.5).
// Extract already refuses to write under data/ by construction; this check
// exists because not touching the user's saves is the product's central
// promise, so a redundant assertion on the critical path is worth its cost.
func assertDataUnchanged(dataDir string, before dataSnapshot) error {
	after, err := snapshotDataDir(dataDir)
	if err != nil {
		return err
	}
	if len(after.entries) != len(before.entries) {
		return errors.New("update: data directory entry count changed")
	}
	names := make([]string, 0, len(after.entries))
	for name := range after.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		was, ok := before.entries[name]
		if !ok {
			return errors.New("update: data directory gained an entry")
		}
		if was != after.entries[name] {
			return errors.New("update: a data file was modified")
		}
	}
	return nil
}

// --- end of pipeline ------------------------------------------------------
