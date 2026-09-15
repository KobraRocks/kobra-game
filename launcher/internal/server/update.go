package server

import (
	"context"

	"kobragames.local/launcher/internal/apitypes"
	"kobragames.local/launcher/internal/kobraerr"
	"kobragames.local/launcher/internal/update"
)

// UpdateCheck implements GET /api/update/check (§19.2). It does nothing unless
// the user explicitly asked: there is no background polling, and no request is
// made when the update channel is "none" or when no patch base URL is
// configured.
//
// The check itself lives in the update package, which is the only package
// permitted to make an outbound request (§3.3); this method supplies the policy
// (channel, base URL, installed release) and maps the result to the wire shape.
func (s *Server) UpdateCheck(ctx context.Context) (apitypes.UpdateStatus, error) {
	// §19.2 step 1, and Updater spec §14.3: the installed release comes from the
	// game's release manifest, falling back to launcher.config.json when it is
	// absent or does not declare one. Under scope: full the archive carries the
	// config and deliberately omits the manifest, so the fallback is the normal
	// post-swap path.
	current := s.release
	if declared := update.InstalledReleaseIn(s.root.GameFolder); declared != "" {
		current = declared
	}
	status := apitypes.UpdateStatus{Current: current}

	if s.cfg.Update.Channel == "none" {
		status.Detail = "Update checks are disabled for this game."
		return status, nil
	}
	if s.cfg.Update.PatchBaseURL == "" {
		status.Detail = "No update server is configured for this game."
		return status, nil
	}

	res, err := update.Check(ctx, s.cfg.Update.PatchBaseURL, current)
	if err != nil {
		// A failed check is reported, not fatal: the game keeps running.
		s.log.Warn("update.check.failed", map[string]any{"reason": "unreachable"})
		status.Detail = "The update server could not be reached."
		return status, nil
	}
	status.Latest = res.Latest
	status.NotesURL = res.NotesURL
	status.Available = res.Available
	status.Detail = res.Detail
	if !status.Available && status.Detail == "" {
		status.Detail = "This is the latest release."
	}
	s.log.Info("update.check", map[string]any{
		"current":   status.Current,
		"latest":    status.Latest,
		"available": status.Available,
	})
	return status, nil
}

// RecordUpdateIntent implements POST /api/update/apply's work (Updater spec
// §6.1). It supplies the channel policy and hands the network work to the
// update package.
func (s *Server) RecordUpdateIntent(ctx context.Context) (update.IntentResult, error) {
	var out update.IntentResult

	if s.cfg.Update.Channel != "patch" {
		return out, kobraerr.Conflict(
			"Updates for this game are installed by hand.", map[string]any{
				"reason":  "manual_channel",
				"channel": s.cfg.Update.Channel,
			})
	}
	if s.cfg.Update.PatchBaseURL == "" {
		return out, kobraerr.Conflict(
			"No update server is configured for this game.", map[string]any{
				"reason": "no_update_server",
			})
	}

	current := s.release
	if declared := update.InstalledReleaseIn(s.root.GameFolder); declared != "" {
		current = declared
	}

	out, err := update.RecordUpdateIntent(ctx, s.cfg.Update.PatchBaseURL, current, s.root.SidecarDir, s.version)
	if err != nil {
		// A failed check is reported to the user, not fatal (FR-UPD-2). The
		// message never carries a URL or a path (§22.4).
		s.log.Warn("update.apply.record_failed", map[string]any{"reason": "unreachable"})
		return out, kobraerr.IO("The update server could not be reached.",
			map[string]any{"reason": "unreachable"}, err)
	}
	return out, nil
}

// CancelUpdateIntent implements POST /api/update/cancel (Updater spec §12.4).
//
// It has two halves because the apply normally runs at startup with no session
// attached: clearing a request that has not started yet, and writing the flag
// that a running apply polls.
func (s *Server) CancelUpdateIntent() (bool, error) {
	cleared := update.ClearUpdateIntent(s.root.SidecarDir)
	if err := update.RequestCancel(s.root.SidecarDir); err != nil {
		return cleared, kobraerr.IO("The update could not be cancelled.",
			map[string]any{"reason": "cancel_write"}, err)
	}
	s.log.Info("update.cancel.requested", map[string]any{"pending": cleared})
	return true, nil
}

// UpdateProgress implements GET /api/update/progress (Updater spec §11.3).
//
// It reads the machine-local records only: no network access, no URL, no hash,
// and no absolute path (spec R11.9).
func (s *Server) UpdateProgress() (apitypes.UpdateProgress, error) {
	pr := update.ReadProgress(s.root.SidecarDir)
	out := apitypes.UpdateProgress{
		Phase:         pr.Phase,
		TargetRelease: pr.TargetRelease,
		FromRelease:   pr.FromRelease,
		Bytes:         pr.Bytes,
		TotalBytes:    pr.TotalBytes,
		Percent:       pr.Percent,
		Cancellable:   pr.Cancellable,
		Message:       pr.Message,
		Reason:        pr.Reason,
		Seq:           pr.Seq,
	}
	// The outcome is attached only when it is newer than the progress record,
	// so a completed update is reported once and an in-flight one is not
	// annotated with a stale ending (spec R11.7).
	if r := update.ReadResult(s.root.SidecarDir); r != nil && r.At >= pr.Updated {
		out.Result = &apitypes.UpdateResult{
			Outcome:        r.Outcome,
			TargetRelease:  r.TargetRelease,
			Release:        r.InstalledRelease,
			Message:        r.Message,
			Reason:         r.Reason,
			RetainedBackup: r.RetainedBackup,
			RequiredBytes:  r.RequiredBytes,
			AvailableBytes: r.AvailableBytes,
			At:             r.At,
		}
	}
	// The revert offer of Packaging spec §9.4 requirement 8: while a retained
	// game.old exists, the shell must be able to offer it. It is reported
	// through the progress record because that is the surface the shell already
	// reads, and both values are Release identifiers, not paths.
	if out.Result == nil && update.HasRetainedBackup(s.root.GameFolder) {
		if prev := update.BackupRelease(s.root.GameFolder); prev != "" {
			out.Result = &apitypes.UpdateResult{
				Outcome:        "retained_backup",
				TargetRelease:  prev,
				RetainedBackup: true,
			}
		}
	}
	return out, nil
}
