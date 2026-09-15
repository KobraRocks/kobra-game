package update

// This file implements the startup pass of Updater spec §7: the one call the
// launcher makes, before the strict layout check and before the instance lock,
// to finish an interrupted swap and then install any requested update.
//
// The ordering is the whole point of §7.1:
//
//	recover  → a crash can leave game/ missing, which paths.Resolve rejects;
//	apply    → may replace game/ entirely, so it must precede the strict check;
//	then     → the launcher may insist on finding a runnable release.
//
// Apply runs before the instance lock, the port, the sidecar port.json, the
// browser and any socket (R7.3), so no client can observe a half-swapped game
// folder. Exclusivity during the apply comes from apply.lock (§17).

import (
	"context"
	"path/filepath"
	"strings"
)

// StartupOptions configures the startup pass.
type StartupOptions struct {
	// GameFolder is the folder derived from the executable (or the dev
	// override). It is needed before paths.Resolve succeeds, because recovery
	// and apply both run while the layout may still be broken.
	GameFolder string
	// SidecarDir is the machine-local state root. An empty value disables the
	// apply, which is the correct degradation when no writable state root could
	// be found (spec §6.2 fallback).
	SidecarDir string
	// LauncherVersion gates launcher_min at apply time (R9.2).
	LauncherVersion string
	// Apply carries the remaining tuning.
	Apply ApplyOptions
}

// StartupResult reports what the startup pass did, for the log.
type StartupResult struct {
	// Recovered is what Recover did to an interrupted swap.
	Recovered RecoveryAction
	// RecoveryErr is non-nil when Recover could not act on a marker it found.
	RecoveryErr error
	// Applied is the apply outcome. Outcome is ApplyNoPending when there was
	// nothing to do.
	Applied ApplyResult
}

// Startup runs the §7 pass. It never returns an error: a failure to recover or
// apply must not stop the launcher from starting with whatever release is on
// disk (R7.9).
func Startup(ctx context.Context, opts StartupOptions) StartupResult {
	var res StartupResult
	if strings.TrimSpace(opts.GameFolder) == "" {
		res.Applied.Outcome = ApplySkipped
		res.Applied.Reason = "no_folder"
		return res
	}

	// 1. §19.4: finish or roll back an interrupted swap, before anything else
	// looks at the layout.
	action, err := Recover(opts.GameFolder, StagingRootFor(opts.SidecarDir))
	res.Recovered = action
	res.RecoveryErr = err
	switch {
	case err != nil:
		// Reported once a logger exists; startup continues so the user sees
		// the real failure rather than a silent exit.
	case action == RecoveryRolledBack:
		// R7.7: the user's request survives a crash-and-rollback cycle, so the
		// marker is kept and the retry budget counts this attempt.
		logWarn("update.swap.rollback", map[string]any{"reason": string(action)})
	case action != RecoveryNone:
		logInfo("update.recovery", map[string]any{"action": string(action)})
	}

	// R15.9: a rolled-back install has no backup to retire, and a stale count
	// would retire the next update's backup prematurely.
	if action == RecoveryRolledBack && opts.SidecarDir != "" {
		ClearStarted(opts.SidecarDir)
	}

	// 2. The apply. Absent marker means no work and no network access.
	if opts.SidecarDir == "" {
		res.Applied.Outcome = ApplySkipped
		res.Applied.Reason = "no_sidecar"
		return res
	}
	pending := ReadPending(opts.SidecarDir)
	if pending == nil {
		res.Applied.Outcome = ApplyNoPending
		return res
	}

	applyOpts := opts.Apply
	if applyOpts.LauncherVersion == "" {
		applyOpts.LauncherVersion = opts.LauncherVersion
	}
	logInfo("update.apply.begin", map[string]any{
		"from":   pending.FromRelease,
		"target": pending.TargetRelease,
	})
	res.Applied = Apply(ctx, opts.GameFolder, opts.SidecarDir, applyOpts)

	// R7.10: every outcome is recorded through the update.* events, which Apply
	// and its helpers emit.
	return res
}

// ConfigPathForFolder names the launcher config for a game folder. It exists so
// the pre-config startup pass can read the game id without paths.Resolve, which
// requires a layout that may not exist yet.
func ConfigPathForFolder(gameFolder string) string {
	return filepath.Join(gameFolder, filepath.FromSlash(configRelPath))
}
