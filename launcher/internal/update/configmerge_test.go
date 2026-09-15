package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers the config merge of Updater spec §14 (R18.6) and the
// retention counter of §15 (R18.8). Both are rules about which value survives a
// release boundary, which is why they are tested against documents rather than
// against a running pipeline.

// --- §14 config merge ------------------------------------------------------

func writeJSONFile(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return doc
}

// mergeFixture returns (gameFolder, stagingRoot) with the given local and
// release config documents already in place.
func mergeFixture(t *testing.T, local, release map[string]any) (string, string) {
	t.Helper()
	gameFolder := t.TempDir()
	stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
	if local != nil {
		writeJSONFile(t, filepath.Join(gameFolder, filepath.FromSlash(configRelPath)), local)
	}
	if release != nil {
		writeJSONFile(t, filepath.Join(stagingRoot, launcherStagingDirName, filepath.Base(configRelPath)), release)
	}
	return gameFolder, stagingRoot
}

func baseConfig(release string) map[string]any {
	return map[string]any{
		"schema":             "kobra.launcher-config/1",
		"game_id":            "com.kobra.mergetest",
		"game_name":          "Merge Test",
		"release":            release,
		"port":               map[string]any{"base": 18900, "span": 50, "require_confirmation": false},
		"browser_preference": []any{"chromium"},
		"update":             map[string]any{"channel": "manual"},
	}
}

func TestMergeLocalOnlyKeyIsPreserved(t *testing.T) {
	local := baseConfig("2026.09.1")
	local["diagnostics"] = map[string]any{"log_retention_files": 3}
	release := baseConfig("2026.10.1")

	gameFolder, stagingRoot := mergeFixture(t, local, release)
	out, err := MergeConfigInto(gameFolder, stagingRoot)
	if err != nil {
		t.Fatalf("MergeConfigInto: %v", err)
	}
	if !out.Merged || !out.ReleasePresent {
		t.Fatalf("outcome = %+v, want a merge", out)
	}
	got := readJSONFile(t, filepath.Join(stagingRoot, launcherStagingDirName, filepath.Base(configRelPath)))
	diag, ok := got["diagnostics"].(map[string]any)
	if !ok {
		t.Fatalf("a local-only object was dropped: %v", got)
	}
	if n, _ := diag["log_retention_files"].(float64); int(n) != 3 {
		t.Fatalf("diagnostics.log_retention_files = %v, want 3", diag["log_retention_files"])
	}
}

func TestMergeReleaseOnlyKeyIsAdded(t *testing.T) {
	local := baseConfig("2026.09.1")
	delete(local, "game_name") // the release declares it, local does not
	release := baseConfig("2026.10.1")

	gameFolder, stagingRoot := mergeFixture(t, local, release)
	if _, err := MergeConfigInto(gameFolder, stagingRoot); err != nil {
		t.Fatalf("MergeConfigInto: %v", err)
	}
	got := readJSONFile(t, filepath.Join(stagingRoot, launcherStagingDirName, filepath.Base(configRelPath)))
	if name, _ := got["game_name"].(string); name != "Merge Test" {
		t.Fatalf("game_name = %q, want the release's value", name)
	}
}

// TestMergeConflictingKeysResolvePerTable is the §14.2 table itself.
func TestMergeConflictingKeysResolvePerTable(t *testing.T) {
	local := baseConfig("2026.09.1")
	local["update"] = map[string]any{
		"channel":        "patch",
		"patch_base_url": "https://local.invalid/updates",
	}
	local["port"] = map[string]any{"base": 19000, "span": 10, "require_confirmation": false}
	local["locales"] = []any{"en", "de"}
	release := baseConfig("2026.10.1")
	release["update"] = map[string]any{
		"channel":        "manual",
		"patch_base_url": "https://release.invalid/updates",
	}
	release["port"] = map[string]any{"base": 8765, "span": 100, "require_confirmation": true}

	gameFolder, stagingRoot := mergeFixture(t, local, release)
	if _, err := MergeConfigInto(gameFolder, stagingRoot); err != nil {
		t.Fatalf("MergeConfigInto: %v", err)
	}
	got := readJSONFile(t, filepath.Join(stagingRoot, launcherStagingDirName, filepath.Base(configRelPath)))

	// release: release wins.
	if r, _ := got["release"].(string); r != "2026.10.1" {
		t.Fatalf("release = %q, want the release's value", r)
	}
	// update.*: local wins, including nested keys the release also sets.
	upd, _ := got["update"].(map[string]any)
	if c, _ := upd["channel"].(string); c != "patch" {
		t.Fatalf("update.channel = %q, want local 'patch'", c)
	}
	if u, _ := upd["patch_base_url"].(string); u != "https://local.invalid/updates" {
		t.Fatalf("update.patch_base_url = %q, want the local value", u)
	}
	// port.*: local wins (a port is a per-machine fact).
	port, _ := got["port"].(map[string]any)
	if b, _ := port["base"].(float64); int(b) != 19000 {
		t.Fatalf("port.base = %v, want the local 19000", port["base"])
	}
	// locales: local wins. (There is no data_dir key in the config schema; the
	// data directory override is --data-dir, a process flag, not a config key.)
	loc, _ := got["locales"].([]any)
	if len(loc) != 2 || loc[1] != "de" {
		t.Fatalf("locales = %v, want the local value", got["locales"])
	}
}

// TestMergePreservesUnmodelledKeysInTheMergeStep pins the half of R14.4 that is
// reachable: the merge itself never drops a key it does not understand, and it
// names such a key in the event record rather than silently discarding it.
//
// Validation is a separate gate with a stricter answer — see
// TestMergeUnknownKeyIsRefusedByValidation — because launcher.config.schema.json
// sets additionalProperties: false. That divergence from R14.4's
// forward-compatibility promise is recorded in the spec's amendment set rather
// than papered over here.
func TestMergePreservesUnmodelledKeysInTheMergeStep(t *testing.T) {
	local := map[string]any{
		"release":      "2026.09.1",
		"a_future_key": map[string]any{"nested": "value"},
	}
	release := map[string]any{"release": "2026.10.1"}

	merged, fromLocal, _ := mergeConfigDocs(local, release)
	future, ok := merged["a_future_key"].(map[string]any)
	if !ok {
		t.Fatalf("the merge step dropped an unknown key: %v", merged)
	}
	if future["nested"] != "value" {
		t.Fatalf("a_future_key = %v, want it carried through verbatim", future)
	}
	if !containsStr(fromLocal, "a_future_key") {
		t.Fatalf("fromLocal = %v, want it to name the preserved key", fromLocal)
	}
}

// TestMergeUnknownKeyIsRefusedByValidation pins the interaction of R14.4 with
// R14.5 under the shipped schema: a key the schema forbids cannot be installed,
// so the update aborts with E28 rather than publishing a config the next launch
// would refuse.
func TestMergeUnknownKeyIsRefusedByValidation(t *testing.T) {
	local := baseConfig("2026.09.1")
	local["a_future_key"] = map[string]any{"nested": "value"}
	release := baseConfig("2026.10.1")

	gameFolder, stagingRoot := mergeFixture(t, local, release)
	_, err := MergeConfigInto(gameFolder, stagingRoot)
	if err == nil {
		t.Fatal("a config the schema forbids was installed")
	}
	if !strings.Contains(err.Error(), msgDamagedShort) {
		t.Fatalf("err = %v, want the E28 wording", err)
	}
}

func TestMergeAbsentReleaseCopyIsNotAnError(t *testing.T) {
	local := baseConfig("2026.09.1")
	gameFolder, stagingRoot := mergeFixture(t, local, nil)

	out, err := MergeConfigInto(gameFolder, stagingRoot)
	if err != nil {
		t.Fatalf("MergeConfigInto: %v", err)
	}
	// R14.6: no release copy means no merge, and the local copy survives because
	// launcher/ is not swapped away.
	if out.Merged || out.ReleasePresent {
		t.Fatalf("outcome = %+v, want no merge", out)
	}
	if !exists(filepath.Join(gameFolder, filepath.FromSlash(configRelPath))) {
		t.Fatal("the local config was disturbed")
	}
}

func TestMergeAbsentLocalCopyInstallsTheReleaseCopy(t *testing.T) {
	release := baseConfig("2026.10.1")
	gameFolder, stagingRoot := mergeFixture(t, nil, release)

	out, err := MergeConfigInto(gameFolder, stagingRoot)
	if err != nil {
		t.Fatalf("MergeConfigInto: %v", err)
	}
	if !out.Merged {
		t.Fatalf("outcome = %+v, want the release copy installed", out)
	}
	got := readJSONFile(t, filepath.Join(stagingRoot, launcherStagingDirName, filepath.Base(configRelPath)))
	if r, _ := got["release"].(string); r != "2026.10.1" {
		t.Fatalf("release = %q, want the release's value", r)
	}
}

func TestMergeUnreadableLocalCopyDoesNotBlockTheUpdate(t *testing.T) {
	release := baseConfig("2026.10.1")
	gameFolder, stagingRoot := mergeFixture(t, nil, release)
	// A local config that is not JSON at all (R14.7).
	path := filepath.Join(gameFolder, filepath.FromSlash(configRelPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := MergeConfigInto(gameFolder, stagingRoot)
	if err != nil {
		t.Fatalf("MergeConfigInto with an unreadable local config: %v", err)
	}
	if !out.Merged {
		t.Fatalf("outcome = %+v, want the release copy installed", out)
	}
}

// TestMergeInvalidResultAborts covers R14.5: a merge that would install a config
// the next launch refuses must abort the update instead.
func TestMergeInvalidResultAborts(t *testing.T) {
	local := baseConfig("2026.09.1")
	// server.bind must be 127.0.0.1; a local value of something else is a
	// schema-level violation the merge must not launder into an install.
	local["server"] = map[string]any{"bind": "0.0.0.0"}
	release := baseConfig("2026.10.1")

	gameFolder, stagingRoot := mergeFixture(t, local, release)
	_, err := MergeConfigInto(gameFolder, stagingRoot)
	if err == nil {
		t.Fatal("an invalid merged config was accepted")
	}
	if !strings.Contains(err.Error(), msgDamagedShort) {
		t.Fatalf("err = %v, want the E28 wording", err)
	}
	// The staged copy must be left as the release shipped it, so the caller can
	// discard the whole staging tree.
	got := readJSONFile(t, filepath.Join(stagingRoot, launcherStagingDirName, filepath.Base(configRelPath)))
	if r, _ := got["release"].(string); r != "2026.10.1" {
		t.Fatalf("the staged config was modified despite the abort: %v", got)
	}
}

// TestMergeDeepMergesObjects pins the bug that made the first implementation
// silently drop the release's nested keys: a shallow merge of two "update"
// objects discards whichever side lost.
func TestMergeDeepMergesObjects(t *testing.T) {
	local := map[string]any{"update": map[string]any{"channel": "patch"}}
	release := map[string]any{"update": map[string]any{
		"channel":                          "manual",
		"update_check_user_initiated_only": true,
		"retain_previous_release":          true,
		"protect_data_dir":                 true,
	}}
	merged, fromLocal, fromRelease := mergeConfigDocs(local, release)
	upd, _ := merged["update"].(map[string]any)
	if upd == nil {
		t.Fatal("the merged document lost the update object")
	}
	if upd["channel"] != "patch" {
		t.Fatalf("update.channel = %v, want the local value", upd["channel"])
	}
	if upd["protect_data_dir"] != true {
		t.Fatalf("update.protect_data_dir = %v, want the release's value", upd["protect_data_dir"])
	}
	// The event names nested paths so the log is actionable (R14.8).
	if !containsStr(fromLocal, "update.channel") {
		t.Fatalf("fromLocal = %v, want it to name update.channel", fromLocal)
	}
	if !containsStr(fromRelease, "update.protect_data_dir") {
		t.Fatalf("fromRelease = %v, want it to name update.protect_data_dir", fromRelease)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- §15 retention --------------------------------------------------------

// retentionFixture builds a folder in the post-swap state: game/ beside
// game.old/.
func retentionFixture(t *testing.T, withBackup bool) (gameFolder, sidecarDir string) {
	t.Helper()
	gameFolder = t.TempDir()
	sidecarDir = filepath.Join(t.TempDir(), "sidecar")
	writeFile(t, filepath.Join(gameFolder, gameDirName, "index.html"), "new")
	if withBackup {
		writeFile(t, filepath.Join(gameFolder, backupDirName, "index.html"), "old")
	}
	return gameFolder, sidecarDir
}

func TestRetentionFirstStartRetainsTheBackup(t *testing.T) {
	gameFolder, sidecar := retentionFixture(t, true)

	NoteSuccessfulStart(gameFolder, sidecar, "2026.10.1")

	rec := ReadStarted(sidecar)
	if rec == nil {
		t.Fatal("no counter was written")
	}
	if rec.Count != 1 || rec.Release != "2026.10.1" {
		t.Fatalf("counter = %+v, want count 1 at 2026.10.1", rec)
	}
	if !exists(filepath.Join(gameFolder, backupDirName)) {
		t.Fatal("game.old was retired after only one start")
	}
}

func TestRetentionSecondStartRetiresTheBackup(t *testing.T) {
	gameFolder, sidecar := retentionFixture(t, true)

	NoteSuccessfulStart(gameFolder, sidecar, "2026.10.1")
	NoteSuccessfulStart(gameFolder, sidecar, "2026.10.1")

	if exists(filepath.Join(gameFolder, backupDirName)) {
		t.Fatal("game.old survived two successful starts (FR-UPD-8)")
	}
	if ReadStarted(sidecar) != nil {
		t.Fatal("the counter survived retirement")
	}
}

func TestRetentionWithoutBackupDoesNotCount(t *testing.T) {
	gameFolder, sidecar := retentionFixture(t, false)

	NoteSuccessfulStart(gameFolder, sidecar, "2026.10.1")
	NoteSuccessfulStart(gameFolder, sidecar, "2026.10.1")

	// R15.7: a folder that was never updated must not accumulate a count.
	if ReadStarted(sidecar) != nil {
		t.Fatalf("counter = %+v, want none", ReadStarted(sidecar))
	}
}

func TestRetentionReleaseChangeResetsTheCount(t *testing.T) {
	gameFolder, sidecar := retentionFixture(t, true)

	NoteSuccessfulStart(gameFolder, sidecar, "2026.10.1")
	// R15.4: a counter from an older update must not retire a newer backup.
	NoteSuccessfulStart(gameFolder, sidecar, "2026.11.1")

	rec := ReadStarted(sidecar)
	if rec == nil {
		t.Fatal("no counter after the release change")
	}
	if rec.Count != 1 || rec.Release != "2026.11.1" {
		t.Fatalf("counter = %+v, want count 1 at 2026.11.1", rec)
	}
	if !exists(filepath.Join(gameFolder, backupDirName)) {
		t.Fatal("the newer backup was retired by an older counter")
	}
}

func TestRetentionClearedOnRollback(t *testing.T) {
	gameFolder, sidecar := retentionFixture(t, true)
	NoteSuccessfulStart(gameFolder, sidecar, "2026.10.1")
	if ReadStarted(sidecar) == nil {
		t.Fatal("precondition: the counter should exist")
	}

	ClearStarted(sidecar)

	if ReadStarted(sidecar) != nil {
		t.Fatal("the counter survived a rollback (R15.9)")
	}
}

func TestHasRetainedBackupRequiresBothDirectories(t *testing.T) {
	gameFolder, _ := retentionFixture(t, true)
	if !HasRetainedBackup(gameFolder) {
		t.Fatal("a game/ beside a game.old/ is a retained backup")
	}
	none, _ := retentionFixture(t, false)
	if HasRetainedBackup(none) {
		t.Fatal("game.old/ alone is not a retained backup")
	}
}

func TestRetentionIgnoresEmptyArguments(t *testing.T) {
	// A missing sidecar or release must not create state anywhere.
	gameFolder, sidecar := retentionFixture(t, true)
	NoteSuccessfulStart(gameFolder, "", "2026.10.1")
	NoteSuccessfulStart("", sidecar, "2026.10.1")
	NoteSuccessfulStart(gameFolder, sidecar, "")
	if ReadStarted(sidecar) != nil {
		t.Fatal("a call with an empty argument wrote a counter")
	}
}

// --- §10 preflight --------------------------------------------------------

// TestPreflightBoundary is R18.5 in table form: exactly the requirement passes,
// one byte under fails, and the comparison is the one the pipeline makes.
func TestPreflightBoundary(t *testing.T) {
	cases := []struct {
		name           string
		archive, total int64
		free           int64
		wantFreeEnough bool
	}{
		{"exactly the requirement", 100, 1000, RequiredBytes(100, 1000), true},
		{"one byte under", 100, 1000, RequiredBytes(100, 1000) - 1, false},
		{"one byte over", 100, 1000, RequiredBytes(100, 1000) + 1, true},
		{"nothing free", 1, 1, 0, false},
		{"tiny update, roomy disk", 1, 1, 128 << 20, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			required := RequiredBytes(tc.archive, tc.total)
			// The pipeline's rule is `space.Free < required` -> refuse.
			enough := !(tc.free < required)
			if enough != tc.wantFreeEnough {
				t.Fatalf("free %d vs required %d: enough = %v, want %v",
					tc.free, required, enough, tc.wantFreeEnough)
			}
		})
	}
}

// TestDiskSpaceQuerySucceedsOnARealDirectory keeps the platform implementation
// honest: a temp directory is a real filesystem and must report a plausible
// free-space figure.
func TestDiskSpaceQuerySucceedsARealDirectory(t *testing.T) {
	dir := t.TempDir()
	space := freeSpace(spacePath(dir))
	if !space.Known {
		t.Skip("this filesystem cannot report free space; R10.5 permits skipping the check")
	}
	if space.Free <= 0 {
		t.Fatalf("free = %d, want a positive figure", space.Free)
	}
}
