package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kobragames.local/launcher/internal/kobraerr"
)

// Files and directories that make up the §19.3/§19.4 swap protocol. The three
// game-folder entries live directly beside data/; the two staging directories
// live under the sidecar update directory (Updater spec §5), because a portable
// game folder must not carry another machine's interrupted download.
const (
	gameDirName    = "game"
	stagingDirName = "game.new"
	backupDirName  = "game.old"
	stateFileName  = "update.state"
	stateTempName  = "update.state.tmp"

	// launcherDirName is the live launcher tree, which a release replaces in
	// place rather than swapping away (Packaging spec §9.3).
	launcherDirName = "launcher"
	// launcherStagingDirName is the staged copy of that tree, inside the
	// staging root (Updater spec §9.2 step 12).
	launcherStagingDirName = "launcher.new"

	// stateStepBegin is the marker's step before any directory move.
	stateStepBegin = "begin"
	// stateStepPromoted is written after game.new/ has been promoted into game/
	// and before the marker is removed. It is the one piece of evidence that
	// distinguishes "the promotion completed, the marker removal was lost" from
	// "the swap was interrupted after the backup move" — states Recover
	// otherwise cannot tell apart, and which it resolves in opposite directions.
	stateStepPromoted = "promoted"
)

const msgSwapFailed = "The update could not be applied. The previous version is still intact."

// RecoveryAction is what Recover did at startup (Appendix A.7).
type RecoveryAction string

const (
	// RecoveryNone: the folders were already consistent; nothing was touched.
	RecoveryNone RecoveryAction = "none"
	// RecoveryRolledForward: the interrupted swap was completed; game/ now
	// holds the staged release.
	RecoveryRolledForward RecoveryAction = "rolled_forward"
	// RecoveryRolledBack: the previous release was restored to game/.
	RecoveryRolledBack RecoveryAction = "rolled_back"
	// RecoveryCleanedStray: debris from an interrupted or completed swap was
	// removed and game/ was left as it was.
	RecoveryCleanedStray RecoveryAction = "cleaned_stray"
)

// updateState is the crash-recovery marker written to <gameFolder>/update.state
// before the first directory move of a swap and removed after the swap
// completes (§19.3 step 5, §19.4). It is a small JSON document:
//
//	{
//	  "schema":  "kobra.update-state/1",
//	  "release": "2026.10.1",
//	  "step":    "begin",
//	  "from":    "game",
//	  "to":      "game.old",
//	  "staging": "game.new",
//	  "started": "2026-10-01T12:00:00Z"
//	}
//
// Field meanings:
//
//	schema  format tag, so a future launcher can recognise an older marker;
//	release the release being installed, used only for logging;
//	step    how far Swap had got when the marker was written: "begin" before
//	        the first move, "promoted" after game.new/ has been promoted into
//	        game/ and before the marker is removed. Recovery is driven by
//	        which directories exist, because the marker's own write can be
//	        interrupted; "promoted" is used only as positive evidence, never
//	        as the primary signal;
//	from    directory renamed away from (game);
//	to      directory renamed to (game.old);
//	staging directory promoted afterwards (game.new);
//	started RFC 3339 UTC instant, for the log only.
//
// Unknown fields are ignored by readState, so a newer launcher's marker is
// still recognised as "a swap was in flight".
type updateState struct {
	Schema  string `json:"schema"`
	Release string `json:"release"`
	Step    string `json:"step"`
	From    string `json:"from"`
	To      string `json:"to"`
	Staging string `json:"staging"`
	Started string `json:"started"`
}

// writeState writes the marker atomically: a temporary file in the same
// directory is fsynced and renamed into place, so recovery either sees the
// complete marker or no marker at all.
func writeState(path string, st updateState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), stateTempName)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// The rename must survive a power loss: Recover's decisions are driven by
	// this marker's existence, so its directory entry needs the same fsync the
	// other writers in this package give theirs.
	syncDir(filepath.Dir(path))
	return nil
}

// readState reads the marker. A missing or unreadable marker returns nil; a
// corrupt one returns a zero-valued document, because §19.4 recovery is driven
// by the marker's existence, not its contents.
func readState(path string) *updateState {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var st updateState
	if err := json.Unmarshal(b, &st); err != nil {
		return &updateState{}
	}
	return &st
}

// removeState deletes the marker and its temporary file. The error is returned
// rather than swallowed: a marker that outlives a completed swap is what makes
// the next Recover arbitrate an ambiguous folder, so callers record the failure.
func removeState(path string) error {
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		err = nil
	}
	_ = os.Remove(filepath.Join(filepath.Dir(path), stateTempName))
	if err == nil {
		syncDir(filepath.Dir(path))
	}
	return err
}

// clearState removes the marker, logging a failure. The swap is already
// complete at every call site, so a failure is recorded, not returned.
func clearState(path string) {
	if err := removeState(path); err != nil {
		logWarn("update.swap.state.remove_failed", map[string]any{"reason": "state_remove"})
	}
}

// Swap performs the directory swap of §19.3 step 5 with the crash-safe
// protocol of §19.4:
//
//  1. a stale game.old/ is discarded (the current game/ becomes the new
//     rollback copy);
//  2. update.state is written — before any directory move;
//  3. game/ → game.old/;
//  4. game.new/ → game/;
//  5. update.state is removed.
//
// release is the release being installed and is used for logging only.
// data/ is never touched (FR-UPD-7).
//
// stagingRoot is the sidecar update directory that holds the staged release
// (Updater spec §5, R5.3). It is a parameter rather than a fixed location
// inside the game folder so that a portable folder never carries another
// machine's staged release.
//
// If step 4 fails, Swap tries to put game.old/ back so the caller can report a
// runnable game (exit code 3, §2.5) without waiting for the next startup. If
// that best-effort rollback also fails, update.state is deliberately left in
// place so Recover can finish the job at the next start.
func Swap(gameFolder, stagingRoot, release string) error {
	if strings.TrimSpace(gameFolder) == "" {
		return kobraerr.IO(msgSwapFailed, map[string]any{"reason": "no_game_folder"}, nil)
	}
	game := filepath.Join(gameFolder, gameDirName)
	staging := stagedGamePath(stagingRoot)
	backup := filepath.Join(gameFolder, backupDirName)
	statePath := filepath.Join(gameFolder, stateFileName)
	if strings.TrimSpace(staging) == "" {
		return kobraerr.IO(msgSwapFailed, map[string]any{"reason": "no_staging_root"}, nil)
	}

	if !dirExists(staging) {
		return kobraerr.IO(msgSwapFailed, map[string]any{"reason": "no_staging"},
			errors.New("update: staging directory is missing"))
	}
	if !dirExists(game) {
		return kobraerr.IO(msgSwapFailed, map[string]any{"reason": "no_game"},
			errors.New("update: game directory is missing"))
	}

	logInfo("update.swap.begin", map[string]any{"release": release})

	st := updateState{
		Schema:  UpdateStateSchema,
		Release: release,
		Step:    stateStepBegin,
		From:    gameDirName,
		To:      backupDirName,
		Staging: stagingDirName,
		Started: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeState(statePath, st); err != nil {
		return kobraerr.IO(msgSwapFailed, map[string]any{"reason": "state_write"}, err)
	}

	// The previous rollback copy is superseded by this swap. This happens
	// after the marker is durable, so a crash here is recoverable.
	if fileExists(backup) {
		if err := os.RemoveAll(backup); err != nil {
			clearState(statePath)
			return kobraerr.IO(msgSwapFailed, map[string]any{"reason": "stale_backup"}, err)
		}
	}

	if err := os.Rename(game, backup); err != nil {
		clearState(statePath)
		logWarn("update.swap.rollback", map[string]any{"release": release, "reason": "backup_move"})
		return kobraerr.IO(msgSwapFailed, map[string]any{"reason": "backup_move"}, err)
	}

	// The promotion is the one move that can cross filesystems: staging lives
	// under the sidecar, which may be on a different volume from the game
	// folder. promoteDir keeps it atomic by copying to a sibling of game/ first.
	degraded, perr := promoteDir(staging, game)
	if perr != nil {
		rbErr := os.Rename(backup, game)
		if rbErr == nil {
			clearState(statePath)
		}
		logWarn("update.swap.rollback", map[string]any{"release": release, "reason": "promote_move"})
		return kobraerr.IO(msgSwapFailed, map[string]any{"reason": "promote_move"},
			errors.Join(perr, rbErr))
	}
	if degraded {
		logWarn("update.swap.cross_device", map[string]any{"release": release, "reason": "staging_volume"})
	}

	// Record that the promotion completed before removing the marker. If the
	// removal is lost, or the process dies in this window, Recover can then
	// prove the swap finished instead of rolling a good install back.
	st.Step = stateStepPromoted
	if err := writeState(statePath, st); err != nil {
		logWarn("update.swap.state.promoted", map[string]any{"reason": "state_write"})
	}
	clearState(statePath)
	logInfo("update.swap.complete", map[string]any{"release": release})
	return nil
}

// Recover implements the §19.4 startup state machine against gameFolder. It
// returns what it did so main can log it.
//
// The four branches, in the order §19.4 states them:
//
//  1. game.new/ exists and game/ does not: complete the swap (game.new →
//     game).
//  2. game.old/ exists and game/ does not: roll back (game.old → game).
//  3. game/ and game.old/ both exist and update.state is present: the swap was
//     interrupted. If game.new/ is intact, roll forward by finishing the swap;
//     otherwise roll back by discarding game/ and restoring game.old/.
//  4. update.state is absent: the swap did not start or already completed.
//     Debris is removed. A game.new/ is always debris. A game.old/ beside a
//     game/ is NOT debris: §19.6/FR-UPD-8 retain it as the rollback copy until
//     the new release has started successfully twice, and Recover has no way
//     to know that count, so it is preserved. (Reported as an interpretation
//     of §19.4 bullet 4, which calls it "stray".)
//
// Anything else (a marker with neither a game/ nor a game.new/ nor a game.old/
// to act on) is reported as an error with the marker left in place for a human
// to inspect; the caller decides whether to stop.
//
// stagingRoot is the sidecar update directory that holds game.new/ (Updater
// spec §5). Passing "" makes the staging directory simply not exist, which is
// the correct reading when no sidecar could be resolved: recovery then rolls
// back rather than fabricating a staging directory it cannot find.
func Recover(gameFolder, stagingRoot string) (RecoveryAction, error) {
	if strings.TrimSpace(gameFolder) == "" {
		return RecoveryNone, kobraerr.IO("The launcher could not check the last update.",
			map[string]any{"reason": "no_game_folder"}, nil)
	}
	game := filepath.Join(gameFolder, gameDirName)
	staging := stagedGamePath(stagingRoot)
	backup := filepath.Join(gameFolder, backupDirName)
	statePath := filepath.Join(gameFolder, stateFileName)

	// An interrupted cross-device promotion can leave a copy beside game/. It is
	// never a decision input — recovery reads game/, game.old/ and game.new/ —
	// so it is removed up front rather than being mistaken for debris later.
	removePromoteDebris(gameFolder)

	statePresent := fileExists(statePath)
	gameOK := dirExists(game)
	newOK := dirExists(staging)
	oldOK := dirExists(backup)

	release := ""
	promoted := false
	if st := readState(statePath); st != nil {
		release = st.Release
		promoted = st.Step == stateStepPromoted
	}

	switch {
	case newOK && !gameOK:
		// §19.4 bullet 1: complete the swap.
		if _, err := promoteDir(staging, game); err != nil {
			return RecoveryNone, recoveryError("recovery_complete", err)
		}
		clearState(statePath)
		logInfo("update.swap.complete", map[string]any{"release": release})
		return RecoveryRolledForward, nil

	case oldOK && !gameOK:
		// §19.4 bullet 2: roll back.
		if _, err := promoteDir(backup, game); err != nil {
			return RecoveryNone, recoveryError("interrupted", err)
		}
		clearState(statePath)
		logWarn("update.swap.rollback", map[string]any{"release": release, "reason": "interrupted_after_backup"})
		return RecoveryRolledBack, nil

	case gameOK && oldOK && statePresent:
		if newOK {
			// §19.4 bullet 3, staging intact: roll forward by finishing the
			// swap exactly as Swap would have.
			if err := os.RemoveAll(backup); err != nil {
				return RecoveryNone, recoveryError("roll_forward", err)
			}
			if err := os.Rename(game, backup); err != nil {
				return RecoveryNone, recoveryError("roll_forward", err)
			}
			if _, err := promoteDir(staging, game); err != nil {
				_ = os.Rename(backup, game) // best effort; the marker stays
				return RecoveryNone, recoveryError("roll_forward", err)
			}
			clearState(statePath)
			logInfo("update.swap.complete", map[string]any{"release": release})
			return RecoveryRolledForward, nil
		}
		if promoted {
			// The marker itself records that game.new/ was promoted into game/
			// and only the marker removal was lost. game/ is the new release:
			// rolling back here would silently undo a completed install, which
			// is exactly the window that makes this state ambiguous.
			clearState(statePath)
			logInfo("update.swap.complete", map[string]any{"release": release})
			return RecoveryRolledForward, nil
		}
		// §19.4 bullet 3, staging gone and no promotion recorded: roll back. The
		// current game/ is the new release whose promotion completed but whose
		// marker was not removed; the safe move is to restore the previous
		// release.
		if err := os.RemoveAll(game); err != nil {
			return RecoveryNone, recoveryError("roll_back", err)
		}
		if _, err := promoteDir(backup, game); err != nil {
			return RecoveryNone, recoveryError("roll_back", err)
		}
		clearState(statePath)
		logWarn("update.swap.rollback", map[string]any{"release": release, "reason": "staging_missing"})
		return RecoveryRolledBack, nil

	case statePresent && !gameOK && !newOK && !oldOK:
		// Nothing to recover from and no runnable game. Leave the marker so a
		// human can see that an update was in flight.
		return RecoveryNone, kobraerr.IO("The last update left the game folder in a state the launcher could not repair.",
			map[string]any{"reason": "unrecoverable"}, nil)

	case statePresent:
		// Interrupted before the first move (game/ is still the installed
		// release), or a marker that no longer matches any arrangement. Abort
		// the interrupted update: drop the staging directory and the marker,
		// and leave the installed release and rollback copy alone.
		cleaned := false
		if newOK {
			if err := os.RemoveAll(staging); err != nil {
				return RecoveryNone, recoveryError("clean_staging", err)
			}
			cleaned = true
		}
		clearState(statePath)
		if cleaned {
			return RecoveryCleanedStray, nil
		}
		return RecoveryNone, nil

	default:
		// §19.4 bullet 4: no marker. A leftover game.new/ is debris from an
		// aborted extraction. game.old/ is preserved (see the doc comment).
		if newOK {
			if err := os.RemoveAll(staging); err != nil {
				return RecoveryNone, recoveryError("clean_staging", err)
			}
			return RecoveryCleanedStray, nil
		}
		return RecoveryNone, nil
	}
}

// recoveryError wraps a recovery failure with the fixed, path-free message the
// caller can show (§22.4).
func recoveryError(reason string, cause error) error {
	return kobraerr.IO("The launcher could not finish or undo the last update.",
		map[string]any{"reason": reason}, cause)
}

// stagedGamePath resolves the staged game tree inside a staging root. An empty
// root yields an empty path, which dirExists reports as absent rather than
// accidentally naming a directory in the working tree.
func stagedGamePath(stagingRoot string) string {
	if strings.TrimSpace(stagingRoot) == "" {
		return ""
	}
	return filepath.Join(stagingRoot, stagingDirName)
}

// StagingRootFor returns the sidecar update directory a game folder's update
// state lives in. It is the single place the launcher derives that path.
func StagingRootFor(sidecarDir string) string {
	if strings.TrimSpace(sidecarDir) == "" {
		return ""
	}
	return filepath.Join(sidecarDir, UpdateDirName)
}
