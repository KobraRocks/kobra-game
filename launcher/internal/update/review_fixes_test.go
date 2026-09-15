package update

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestPreflightRefusalRecordsByteCounts drives the REAL disk preflight and
// asserts the R10.6 record. The preflight had no test that executed it: the
// existing boundary tests re-implemented `free < required` inside the test file,
// so the refusal path — and its missing byte counts — were never run.
func TestPreflightRefusalRecordsByteCounts(t *testing.T) {
	f := newApplyFixture(t, nil)
	f.record(t)

	orig := freeSpaceFn
	freeSpaceFn = func(string) DiskSpace { return DiskSpace{Free: 1, Known: true} }
	defer func() { freeSpaceFn = orig }()

	res := Apply(context.Background(), f.GameFolder, f.SidecarDir, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyFailed || res.Reason != "insufficient_space" {
		t.Fatalf("apply = %+v, want failed/insufficient_space", res)
	}
	if ReadPending(f.SidecarDir) == nil {
		t.Error("the marker was cleared; R6.5 requires it so the user can retry after freeing space")
	}

	got := ReadResult(f.SidecarDir)
	if got == nil {
		t.Fatal("no result.json was written")
	}
	if got.RequiredBytes <= 0 {
		t.Errorf("result.json required_bytes = %d, want a positive requirement (R10.6)", got.RequiredBytes)
	}
	if got.AvailableBytes != 1 {
		t.Errorf("result.json available_bytes = %d, want the queried 1 (R10.6)", got.AvailableBytes)
	}
}

// TestPreflightUnknownSpaceDoesNotRefuse covers R10.5: an unqueryable
// filesystem must not refuse the update. Only the preflight is under test, so
// the apply is expected to move past it and fail later, on the download.
func TestPreflightUnknownSpaceDoesNotRefuse(t *testing.T) {
	f := newApplyFixture(t, nil)
	f.record(t)

	orig := freeSpaceFn
	freeSpaceFn = func(string) DiskSpace { return DiskSpace{} } // Known: false
	defer func() { freeSpaceFn = orig }()

	res := Apply(context.Background(), f.GameFolder, f.SidecarDir, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Reason == "insufficient_space" {
		t.Fatal("an unknown free-space figure refused the update (R10.5)")
	}
}

// TestBackupReleaseNamesTheBackup is the regression test for the revert offer:
// BackupRelease used to read update.state, whose Release field is the release
// being INSTALLED and which is deleted after every successful swap — so the
// offer either vanished or named the release already installed.
func TestBackupReleaseNamesTheBackup(t *testing.T) {
	folder := t.TempDir()
	writeFile(t, filepath.Join(folder, gameDirName, "index.html"), "new")
	writeFile(t, filepath.Join(folder, backupDirName, "release.manifest.json"), `{"release":"2026.09.1"}`)

	if got := BackupRelease(folder); got != "2026.09.1" {
		t.Errorf("BackupRelease = %q, want the release held by game.old", got)
	}

	// The full archive omits game/release.manifest.json, so the backup's own
	// config is the fallback.
	if err := os.Remove(filepath.Join(folder, backupDirName, "release.manifest.json")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(folder, backupDirName, "launcher", "launcher.config.json"),
		`{"schema":"kobra.launcher-config/1","game_id":"com.kobra.x","release":"2026.08.1"}`)
	if got := BackupRelease(folder); got != "2026.08.1" {
		t.Errorf("BackupRelease = %q, want the backup config's release", got)
	}

	// No game.old/ means no offer at all.
	if err := os.RemoveAll(filepath.Join(folder, backupDirName)); err != nil {
		t.Fatal(err)
	}
	if got := BackupRelease(folder); got != "" {
		t.Errorf("BackupRelease with no backup = %q, want \"\"", got)
	}
}

// TestMergeReleaseOwnedKeysFollowTheRelease covers the R14.2 owner table: it
// lists game_id, game_name, mime_types, locales and schema as release-owned
// alongside release, but only "release" was implemented, so a local document
// could keep the previous release's identity, MIME types and locale list.
func TestMergeReleaseOwnedKeysFollowTheRelease(t *testing.T) {
	local := map[string]any{
		"schema":     "kobra.launcher-config/1",
		"game_id":    "com.local",
		"game_name":  "Local Name",
		"mime_types": map[string]any{"local": "x/local"},
		"locales":    []any{"fr"},
		"release":    "2026.09.1",
		"data_dir":   "/home/someone/games",
	}
	release := map[string]any{
		"schema":     "kobra.launcher-config/1",
		"game_id":    "com.kobra.x",
		"game_name":  "Release Name",
		"mime_types": map[string]any{"release": "x/release"},
		"locales":    []any{"en"},
		"release":    "2026.10.1",
	}

	merged, fromLocal, fromRelease := mergeConfigDocs(local, release)

	for _, k := range []string{"schema", "game_id", "game_name", "mime_types", "locales", "release"} {
		if !reflect.DeepEqual(merged[k], release[k]) {
			t.Errorf("merged[%q] = %v, want the release value %v", k, merged[k], release[k])
		}
		if !containsStr(fromRelease, k) {
			t.Errorf("%q is not attributed to the release: %v", k, fromRelease)
		}
	}
	// A key only the local document declares is still preserved (R14.4), and
	// data_dir MUST stay local: it is a path on this machine (R14.2).
	if merged["data_dir"] != "/home/someone/games" {
		t.Errorf("a local-only key was lost: %v", merged["data_dir"])
	}
	if !containsStr(fromLocal, "data_dir") {
		t.Errorf("data_dir is not attributed to the local document: %v", fromLocal)
	}
}
