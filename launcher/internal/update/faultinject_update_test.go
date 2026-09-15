//go:build kobra_faultinject

package update

import (
	"path/filepath"
	"strings"
	"testing"

	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/faultinject"
)

// TestPromoteFallsBackOnEXDEV is scenario 3 of §26.4 for the updater.
//
// The promotion is the one move that can cross filesystems — staging lives under
// the sidecar — and before the harness existed this could only be exercised on a
// machine that happened to have two. Forcing the rename failure makes the copy
// path deterministic, and the test asserts the whole result: a complete tree, the
// previous release kept as the rollback copy, no leftover copy, and the
// degradation in the log.
func TestPromoteFallsBackOnEXDEV(t *testing.T) {
	folder := t.TempDir()
	stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
	writeFile(t, filepath.Join(folder, gameDirName, "index.html"), "old")
	writeFile(t, filepath.Join(folder, gameDirName, "assets", "a.txt"), "old-asset")
	writeFile(t, filepath.Join(stagingRoot, stagingDirName, "index.html"), "new")
	writeFile(t, filepath.Join(stagingRoot, stagingDirName, "assets", "a.txt"), "new-asset")

	log, err := diagnostics.Open(diagnostics.Options{
		LogDir: filepath.Join(t.TempDir(), "logs"),
		// Warnings are the events under assertion; the zero level is error-only.
		Level: diagnostics.LevelDebug,
	})
	if err != nil {
		t.Fatal(err)
	}
	SetLogger(log)
	t.Cleanup(func() {
		SetLogger(nil)
		_ = log.Close()
	})

	faultinject.ArmForTest(faultinject.UpdatePromoteRename + "=exdev")
	t.Cleanup(func() { faultinject.ArmForTest("") })

	if err := Swap(folder, stagingRoot, "2026.10.1"); err != nil {
		t.Fatalf("Swap over the cross-device fallback: %v", err)
	}

	if got := readFile(t, filepath.Join(folder, gameDirName, "index.html")); got != "new" {
		t.Errorf("promoted game/index.html = %q, want the staged release", got)
	}
	if got := readFile(t, filepath.Join(folder, gameDirName, "assets", "a.txt")); got != "new-asset" {
		t.Errorf("promoted nested file = %q, want new-asset", got)
	}
	if got := readFile(t, filepath.Join(folder, backupDirName, "index.html")); got != "old" {
		t.Errorf("game.old/index.html = %q, want the previous release", got)
	}
	if exists(filepath.Join(stagingRoot, stagingDirName)) {
		t.Error("staging was not consumed by the fallback promotion")
	}
	debris, _ := filepath.Glob(filepath.Join(folder, promoteTempPrefix(filepath.Join(folder, gameDirName))+"*"))
	if len(debris) != 0 {
		t.Errorf("the fallback left promotion debris: %v", debris)
	}
	logged := strings.Join(log.Tail(0), "\n")
	if !strings.Contains(logged, "update.swap.cross_device") {
		t.Errorf("the degraded promotion was not logged: %s", logged)
	}
	if !strings.Contains(logged, `"reason":"staging_volume"`) {
		t.Errorf("the degradation event carries the wrong reason: %s", logged)
	}
}

// TestPromoteStaysAtomicWhenTheRenameWorks is the control: with nothing injected
// the fallback must not run at all, which is what keeps the degraded path a
// fallback rather than the normal one.
func TestPromoteStaysAtomicWhenTheRenameWorks(t *testing.T) {
	folder := t.TempDir()
	stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
	writeFile(t, filepath.Join(folder, gameDirName, "index.html"), "old")
	writeFile(t, filepath.Join(stagingRoot, stagingDirName, "index.html"), "new")

	log, err := diagnostics.Open(diagnostics.Options{
		LogDir: filepath.Join(t.TempDir(), "logs"),
		// Warnings are the events under assertion; the zero level is error-only.
		Level: diagnostics.LevelDebug,
	})
	if err != nil {
		t.Fatal(err)
	}
	SetLogger(log)
	t.Cleanup(func() {
		SetLogger(nil)
		_ = log.Close()
	})

	faultinject.ArmForTest("")
	if err := Swap(folder, stagingRoot, "2026.10.1"); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if logged := strings.Join(log.Tail(0), "\n"); strings.Contains(logged, "update.swap.cross_device") {
		t.Errorf("the cross-device path ran without an injected failure: %s", logged)
	}
}
