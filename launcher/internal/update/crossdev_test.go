package update

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSwapCrossDevicePromotion is the regression test for updates being
// impossible when the sidecar (machine-local state) is on a different
// filesystem from the game folder: the promotion rename fails with EXDEV, so
// promoteDir has to copy onto the destination's filesystem and rename there.
//
// The two roots are only on different filesystems on some machines, so each
// candidate is probed with the very operation under test and the test is skipped
// when no split layout is available.
func TestSwapCrossDevicePromotion(t *testing.T) {
	base := t.TempDir() // usually a tmpfs on Linux
	other := ""
	for _, root := range []string{".", os.Getenv("HOME"), "/var/tmp"} {
		if root == "" {
			continue
		}
		d, err := os.MkdirTemp(root, "kobra-crossdev-")
		if err != nil {
			continue
		}
		probe := filepath.Join(base, "probe")
		if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
			_ = os.RemoveAll(d)
			t.Fatal(err)
		}
		rerr := os.Rename(probe, filepath.Join(d, "probe"))
		if rerr == nil {
			_ = os.RemoveAll(d) // same filesystem: try the next candidate
			continue
		}
		if isCrossDevice(rerr) {
			other = d
			break
		}
		_ = os.RemoveAll(d)
	}
	if other == "" {
		t.Skip("no second filesystem is available to exercise a cross-device promotion")
	}
	defer func() { _ = os.RemoveAll(other) }()

	gameFolder := filepath.Join(base, "Pkg")
	stagingRoot := filepath.Join(other, UpdateDirName)
	writeFile(t, filepath.Join(gameFolder, gameDirName, "index.html"), "old")
	writeFile(t, filepath.Join(stagingRoot, stagingDirName, "index.html"), "new")
	writeFile(t, filepath.Join(stagingRoot, stagingDirName, "assets", "a.txt"), "asset")

	if err := Swap(gameFolder, stagingRoot, "2026.10.1"); err != nil {
		t.Fatalf("Swap across filesystems: %v", err)
	}
	if got := readFile(t, filepath.Join(gameFolder, gameDirName, "index.html")); got != "new" {
		t.Errorf("promoted game/index.html = %q, want the staged release", got)
	}
	if got := readFile(t, filepath.Join(gameFolder, gameDirName, "assets", "a.txt")); got != "asset" {
		t.Errorf("promoted nested file = %q, want asset", got)
	}
	if got := readFile(t, filepath.Join(gameFolder, backupDirName, "index.html")); got != "old" {
		t.Errorf("game.old/index.html = %q, want the previous release", got)
	}
	if exists(filepath.Join(stagingRoot, stagingDirName)) {
		t.Error("game.new still exists after a cross-device promotion")
	}
	if exists(filepath.Join(gameFolder, stateFileName)) {
		t.Error("update.state still exists after a successful cross-device swap")
	}
	// No promotion copy may be left behind on the destination filesystem.
	pattern := filepath.Join(gameFolder, promoteTempPrefix(filepath.Join(gameFolder, gameDirName))+"*")
	if matches, _ := filepath.Glob(pattern); len(matches) != 0 {
		t.Errorf("promotion left debris: %v", matches)
	}
}

// TestRecoverMatrixPromotedMarker contrasts the two readings of
// "game/ + game.old/ + marker, staging gone": with step "begin" (the documented
// interpretation) Recover rolls back, but with step "promoted" the marker itself
// proves the promotion completed, so rolling back would silently undo a
// successful install.
func TestRecoverMatrixPromotedMarker(t *testing.T) {
	fixture := func(t *testing.T, step string) (string, string) {
		t.Helper()
		folder := t.TempDir()
		writeFile(t, filepath.Join(folder, gameDirName, "index.html"), "new")
		writeFile(t, filepath.Join(folder, backupDirName, "index.html"), "old")
		st := updateState{
			Schema:  UpdateStateSchema,
			Release: "2026.10.1",
			Step:    step,
			From:    gameDirName,
			To:      backupDirName,
			Staging: stagingDirName,
		}
		if err := writeState(filepath.Join(folder, stateFileName), st); err != nil {
			t.Fatal(err)
		}
		return folder, filepath.Join(t.TempDir(), UpdateDirName)
	}

	t.Run("promoted finishes the swap", func(t *testing.T) {
		folder, stagingRoot := fixture(t, stateStepPromoted)
		action, err := Recover(folder, stagingRoot)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if action != RecoveryRolledForward {
			t.Errorf("action = %q, want %q", action, RecoveryRolledForward)
		}
		if got := readFile(t, filepath.Join(folder, gameDirName, "index.html")); got != "new" {
			t.Errorf("game/index.html = %q, want the installed release to survive", got)
		}
		if got := readFile(t, filepath.Join(folder, backupDirName, "index.html")); got != "old" {
			t.Errorf("game.old/index.html = %q, want the rollback copy kept", got)
		}
		if exists(filepath.Join(folder, stateFileName)) {
			t.Error("the marker was not cleared")
		}
	})

	t.Run("begin still rolls back", func(t *testing.T) {
		folder, stagingRoot := fixture(t, stateStepBegin)
		action, err := Recover(folder, stagingRoot)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if action != RecoveryRolledBack {
			t.Errorf("action = %q, want %q", action, RecoveryRolledBack)
		}
		if got := readFile(t, filepath.Join(folder, gameDirName, "index.html")); got != "old" {
			t.Errorf("game/index.html = %q, want the previous release restored", got)
		}
	})
}
