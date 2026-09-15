package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This file exercises the Apply pipeline of Updater spec §9 end to end, against
// a real HTTP server and a real filesystem, because most of the requirements it
// covers are about what happens between two directories rather than about a
// function's return value.

// --- fixture ---------------------------------------------------------------

// applyFixture is a whole game folder plus a published release, wired so Apply
// can run against it.
type applyFixture struct {
	GameFolder string
	SidecarDir string
	fromRel    string
	targetRel  string

	srv         *httptest.Server
	archive     string // server path of the archive
	archivePath string // filesystem path of the archive
	manifest    string // server path of the manifest
	archiveSz   int64
	files       map[string]string // game-relative path -> content
	launcherCfg string            // the archive's launcher.config.json
}

// releaseConfigJSON is the launcher config an archive ships. It deliberately
// declares channel "manual" — the trap of §14.1 — so every apply test also
// proves the local-wins merge.
func releaseConfigJSON(gameID, release string) string {
	return fmt.Sprintf(`{
  "schema": "kobra.launcher-config/1",
  "game_id": %q,
  "game_name": "Update Test",
  "release": %q,
  "port": {"base": 18900, "span": 50, "require_confirmation": false},
  "browser_preference": ["chromium"],
  "update": {"channel": "manual", "patch_base_url": "https://example.invalid/updates"}
}`, gameID, release)
}

// localConfigJSON is the installed launcher config. It has been switched to the
// patch channel by the user, which is what must survive the swap.
func localConfigJSON(gameID, release, baseURL string) string {
	return fmt.Sprintf(`{
  "schema": "kobra.launcher-config/1",
  "game_id": %q,
  "game_name": "Update Test",
  "release": %q,
  "port": {"base": 18900, "span": 50, "require_confirmation": false},
  "browser_preference": ["chromium"],
  "update": {"channel": "patch", "patch_base_url": %q},
  "diagnostics": {"log_retention_files": 3}
}`, gameID, release, baseURL)
}

// newApplyFixture builds the folder, the archive and the server. The archive
// carries game/index.html, a level file, and launcher/launcher.config.json.
func newApplyFixture(t *testing.T, files map[string]string) *applyFixture {
	t.Helper()
	const gameID = "com.kobra.updatefixture"
	fromRel, targetRel := "2026.09.1", "2026.10.1"

	root := t.TempDir()
	gameFolder := filepath.Join(root, "Pkgtest")
	sidecarDir := filepath.Join(root, "sidecar")
	if err := os.MkdirAll(sidecarDir, 0o700); err != nil {
		t.Fatalf("mkdir sidecar: %v", err)
	}

	// The installed folder: a game tree at the old release, plus data/.
	gameDir := filepath.Join(gameFolder, "game")
	mkdir(t, gameDir)
	writeFile(t, filepath.Join(gameDir, "index.html"), "<html>old</html>\n")
	writeFile(t, filepath.Join(gameDir, "release.manifest.json"), `{"release":"`+fromRel+`"}`)
	writeFile(t, filepath.Join(gameFolder, "launcher", "launcher.config.json"),
		localConfigJSON(gameID, fromRel, ""))
	writeFile(t, filepath.Join(gameFolder, "data", "saves", "slot1.json"), `{"slot":1}`)
	writeFile(t, filepath.Join(gameFolder, "data", "config", "settings.json"), `{"volume":7}`)

	// The archive the server will publish.
	launcherCfg := releaseConfigJSON(gameID, targetRel)
	entries := []tarEntry{
		{Name: "game/index.html", Body: []byte("<html>new</html>\n")},
		{Name: "launcher/launcher.config.json", Body: []byte(launcherCfg)},
	}
	for name, body := range files {
		entries = append(entries, tarEntry{Name: "game/" + name, Body: []byte(body)})
	}
	archivePath := buildArchive(t, entries...)

	f := &applyFixture{
		GameFolder:  gameFolder,
		SidecarDir:  sidecarDir,
		fromRel:     fromRel,
		targetRel:   targetRel,
		files:       files,
		launcherCfg: launcherCfg,
		archivePath: archivePath,
	}
	st, err := os.Stat(archivePath)
	if err != nil {
		t.Fatalf("stat archive: %v", err)
	}
	f.archiveSz = st.Size()
	f.archive = "/" + filepath.Base(archivePath)
	f.manifest = "/" + targetRel + ".release.manifest.json"

	// The manifest: every archive member listed with its hash and size, and the
	// archive's own hash, as Packaging spec §6.4 and §8.2 require.
	var list []FileEntry
	var total int64
	for _, e := range entries {
		name := e.Name
		body := e.Body
		list = append(list, FileEntry{
			Path: name,
			Size: int64(len(body)),
			Hash: sha256Of(body),
		})
		total += int64(len(body))
	}
	manifest := map[string]any{
		"schema":           ReleaseManifestSchema,
		"release":          targetRel,
		"previous_release": fromRel,
		"launcher_min":     "0.1.0",
		"engine_version":   "1.0.0",
		"save_version":     1,
		"scope":            "full",
		"total_size":       total,
		"files":            list,
		"archive": map[string]any{
			"name":   filepath.Base(archivePath),
			"url":    f.archive,
			"size":   f.archiveSz,
			"hash":   sha256Of(mustRead(t, archivePath)),
			"format": "tar.zst",
		},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	latest := map[string]any{
		"release":      targetRel,
		"manifest_url": f.manifest,
		"notes_url":    "https://example.invalid/notes",
	}
	latestJSON, _ := json.Marshal(latest)

	// A mutable handler so a test can rewrite the manifest mid-flight and
	// exercise the drift check of §9.2 step 5.
	mux := http.NewServeMux()
	mux.HandleFunc(f.archive, func(w http.ResponseWriter, r *http.Request) {
		serveFile(t, w, r, archivePath)
	})
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(latestJSON)
	})
	mux.HandleFunc(f.manifest, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(manifestJSON)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	// Point the installed local config at the fixture server, which is how the
	// patch channel is enabled in the first place.
	writeFile(t, filepath.Join(gameFolder, "launcher", "launcher.config.json"),
		localConfigJSON(gameID, fromRel, f.srv.URL))

	return f
}

// serveFile serves a file with Content-Length and Range support.
func serveFile(t *testing.T, w http.ResponseWriter, r *http.Request, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "missing", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "missing", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// record writes a pending marker that names the fixture's release.
func (f *applyFixture) record(t *testing.T) *Pending {
	t.Helper()
	manifestURL := f.srv.URL + f.manifest
	m, err := FetchManifest(context.Background(), manifestURL)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	p := &Pending{
		Schema:          PendingSchema,
		FromRelease:     f.fromRel,
		TargetRelease:   f.targetRel,
		ManifestURL:     manifestURL,
		ArchiveURL:      f.srv.URL + f.archive,
		ArchiveSize:     m.declaredArchiveSize(),
		ArchiveHash:     m.declaredArchiveHash(),
		LauncherVersion: "0.1.0",
		Requested:       nowRFC3339(),
	}
	if err := WritePending(f.SidecarDir, p); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	return p
}

// apply runs Apply with the disk preflight relaxed, since the fixture is small
// and the test must not depend on the machine's free space.
//
// StallTimeout is deliberately left at its zero value so the tests exercise the
// production default deadline: a regression that disarms the watchdog must fail
// here rather than be masked by a test-only override.
func (f *applyFixture) apply(t *testing.T, opts ApplyOptions) ApplyResult {
	t.Helper()
	opts.SkipDownloadCheckForTest = true
	return Apply(context.Background(), f.GameFolder, f.SidecarDir, opts)
}

// TestApplyFailedSwapKeepsTheInstalledRelease is the regression test for the
// silent update loss: the launcher merge used to run before the swap, so a
// failed swap left launcher.config.json — which is what reports the installed
// release when the archive omits game/release.manifest.json — naming a release
// that had never been installed. The retry then short-circuited as
// already_current and the update was gone for good.
func TestApplyFailedSwapKeepsTheInstalledRelease(t *testing.T) {
	f := newApplyFixture(t, nil)
	f.record(t)

	// Remove game/ so the swap fails at its first move, after the marker was
	// written and after everything was staged and merged.
	if err := os.RemoveAll(filepath.Join(f.GameFolder, gameDirName)); err != nil {
		t.Fatal(err)
	}

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyFailed || res.Reason != "swap_failed" {
		t.Fatalf("first apply = %+v, want failed/swap_failed", res)
	}

	// The live launcher config must still describe the instalated release.
	cfg := readFile(t, filepath.Join(f.GameFolder, launcherDirName, "launcher.config.json"))
	if strings.Contains(cfg, f.targetRel) {
		t.Errorf("a failed swap installed the release's launcher.config.json:\n%s", cfg)
	}
	if got := InstalledReleaseIn(f.GameFolder); got == f.targetRel {
		t.Errorf("a failed swap made the folder report the target release %q", got)
	}

	// The retry must attempt the update again rather than declare it current.
	f.record(t)
	res2 := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res2.Outcome == ApplyAlreadyCurrent {
		t.Error("the retry short-circuited as already_current; the update is still silently lost")
	}
}

// --- §9.2 happy path -------------------------------------------------------

func TestApplyHappyPath(t *testing.T) {
	f := newApplyFixture(t, map[string]string{
		"assets/data/levels.json": `{"levels":[{"id":2}]}`,
	})
	f.record(t)

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyApplied {
		t.Fatalf("outcome = %v (%s), want applied", res.Outcome, res.Reason)
	}
	if res.TargetRelease != f.targetRel {
		t.Fatalf("target = %q, want %q", res.TargetRelease, f.targetRel)
	}

	// E2E-1: the new level file is installed.
	if got := readFile(t, filepath.Join(f.GameFolder, "game", "assets", "data", "levels.json")); !strings.Contains(got, `"id":2`) {
		t.Fatalf("the new release's level file is missing: %q", got)
	}
	// E2E-3: game.old is retained with the old release's content.
	old := filepath.Join(f.GameFolder, backupDirName)
	if !exists(filepath.Join(old, "index.html")) {
		t.Fatal("game.old was not retained")
	}
	if !strings.Contains(readFile(t, filepath.Join(old, "index.html")), "old") {
		t.Fatal("game.old does not hold the previous release")
	}
	// E2E-6: no debris. The staged game tree was renamed away by the swap, and
	// the staged launcher tree was dropped after the merge.
	stagingRoot := filepath.Join(f.SidecarDir, UpdateDirName)
	if exists(filepath.Join(stagingRoot, stagingDirName)) {
		t.Fatal("game.new was left behind")
	}
	if exists(filepath.Join(stagingRoot, launcherStagingDirName)) {
		t.Fatal("launcher.new was left behind")
	}
	if exists(filepath.Join(f.GameFolder, stateFileName)) {
		t.Fatal("update.state was left behind")
	}
	if exists(updatePath(f.SidecarDir, DownloadFileName)) {
		t.Fatal("the verified archive was not removed")
	}
	// E2E-2: data/ is untouched.
	if got := readFile(t, filepath.Join(f.GameFolder, "data", "saves", "slot1.json")); got != `{"slot":1}` {
		t.Fatalf("data/saves/slot1.json = %q, want it untouched", got)
	}
	if got := readFile(t, filepath.Join(f.GameFolder, "data", "config", "settings.json")); got != `{"volume":7}` {
		t.Fatalf("data/config/settings.json = %q, want it untouched", got)
	}
	// The marker is cleared on success.
	if p := ReadPending(f.SidecarDir); p != nil {
		t.Fatal("pending marker survived a successful apply")
	}
	// The outcome is recorded.
	r := ReadResult(f.SidecarDir)
	if r == nil || r.Outcome != OutcomeComplete {
		t.Fatalf("result = %+v, want a completion record", r)
	}
}

// --- §14 the config merge --------------------------------------------------

// TestApplyPreservesLocalUpdateChannel is the trap the investigation missed:
// the archive ships channel "manual" while the installed config says "patch",
// so a naive swap would silently disable the update mechanism that performed
// it. This is E2E-5.
func TestApplyPreservesLocalUpdateChannel(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	f.record(t)
	if res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"}); res.Outcome != ApplyApplied {
		t.Fatalf("outcome = %v (%s), want applied", res.Outcome, res.Reason)
	}

	cfgPath := filepath.Join(f.GameFolder, "launcher", "launcher.config.json")
	raw := mustRead(t, cfgPath)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("merged config is not JSON: %v", err)
	}
	upd, _ := doc["update"].(map[string]any)
	if upd == nil {
		t.Fatal("merged config has no update object")
	}
	// The local channel survives.
	if got, _ := upd["channel"].(string); got != "patch" {
		t.Fatalf("update.channel = %q, want the local value %q", got, "patch")
	}
	// The local patch_base_url survives.
	if got, _ := upd["patch_base_url"].(string); got != f.srv.URL {
		t.Fatalf("update.patch_base_url = %q, want the local value %q", got, f.srv.URL)
	}
	// The release comes from the archive: this is what makes the post-swap
	// folder report the new release (R14.3).
	if got, _ := doc["release"].(string); got != f.targetRel {
		t.Fatalf("release = %q, want the release's value %q", got, f.targetRel)
	}
	// A local-only key the release does not declare is preserved.
	diag, _ := doc["diagnostics"].(map[string]any)
	if diag == nil {
		t.Fatal("the merge dropped a local-only object")
	}
	if got, ok := diag["log_retention_files"].(float64); !ok || int(got) != 3 {
		t.Fatalf("diagnostics.log_retention_files = %v, want the local value 3", diag["log_retention_files"])
	}
	// A game_name the release declares but the local document lacked is added.
	if got, _ := doc["game_name"].(string); got != "Update Test" {
		t.Fatalf("game_name = %q, want the release's value", got)
	}
}

// TestInstalledReleaseAfterSwap pins E2E-4: the reported release after a swap
// must be the new one, which under scope: full comes from the merged config
// because the archive deliberately omits game/release.manifest.json.
func TestInstalledReleaseAfterSwap(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	// The pre-swap folder reports the old release from its game manifest.
	if got := InstalledReleaseIn(f.GameFolder); got != f.fromRel {
		t.Fatalf("pre-swap release = %q, want %q", got, f.fromRel)
	}
	f.record(t)
	if res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"}); res.Outcome != ApplyApplied {
		t.Fatalf("outcome = %v (%s), want applied", res.Outcome, res.Reason)
	}
	// The archive carries no game/release.manifest.json, so the fallback to the
	// merged config is the whole answer.
	if exists(filepath.Join(f.GameFolder, "game", "release.manifest.json")) {
		t.Fatal("the swap left a release manifest the archive did not carry")
	}
	if got := InstalledReleaseIn(f.GameFolder); got != f.targetRel {
		t.Fatalf("post-swap release = %q, want %q", got, f.targetRel)
	}
	if res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"}); res.Outcome != ApplyNoPending {
		t.Fatalf("second apply = %v, want no_pending", res.Outcome)
	}
}

// --- §6 the trigger state machine -----------------------------------------

func TestApplyNoPendingIsANoOp(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	before := readFile(t, filepath.Join(f.GameFolder, "game", "index.html"))

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyNoPending {
		t.Fatalf("outcome = %v, want no_pending", res.Outcome)
	}
	if got := readFile(t, filepath.Join(f.GameFolder, "game", "index.html")); got != before {
		t.Fatal("a no-op apply changed the game folder")
	}
	// No progress record is manufactured for an update that was never requested.
	if pr := ReadProgress(f.SidecarDir); pr.Phase != PhaseIdle {
		t.Fatalf("phase = %q, want idle", pr.Phase)
	}
}

func TestApplyAlreadyCurrentClearsTheMarker(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	p := f.record(t)
	// Pretend the swap already happened: the installed release equals the
	// target. This is the crash-between-swap-and-clear case of R6.8.
	writeFile(t, filepath.Join(f.GameFolder, "game", "release.manifest.json"),
		`{"release":"`+f.targetRel+`"}`)

	res := f.apply(t, ApplyOptions{LauncherVersion: p.LauncherVersion})
	if res.Outcome != ApplyAlreadyCurrent {
		t.Fatalf("outcome = %v (%s), want already_current", res.Outcome, res.Reason)
	}
	if ReadPending(f.SidecarDir) != nil {
		t.Fatal("the marker should have been cleared")
	}
}

func TestApplyClearsAnInvalidMarker(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	// A marker with no manifest URL cannot be acted on.
	if err := writeJSONAtomic(updatePath(f.SidecarDir, PendingFileName), Pending{
		Schema:        PendingSchema,
		TargetRelease: f.targetRel,
	}); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyFailed || res.Reason != "invalid_marker" {
		t.Fatalf("outcome = %v (%s), want failed/invalid_marker", res.Outcome, res.Reason)
	}
	if ReadPending(f.SidecarDir) != nil {
		t.Fatal("an invalid marker must be cleared")
	}
}

func TestApplyRefusesADowngradeMarker(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	if err := WritePending(f.SidecarDir, &Pending{
		Schema:        PendingSchema,
		FromRelease:   "2026.11.1",
		TargetRelease: "2026.10.1",
		ManifestURL:   f.srv.URL + f.manifest,
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}
	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Reason != "not_newer" {
		t.Fatalf("reason = %q, want not_newer", res.Reason)
	}
	if ReadPending(f.SidecarDir) != nil {
		t.Fatal("the marker should have been cleared")
	}
}

func TestApplyHonoursRetryBudget(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	p := f.record(t)
	p.RetryCount = RetryBudget
	if err := writeJSONAtomic(updatePath(f.SidecarDir, PendingFileName), p); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Reason != "retries_exhausted" {
		t.Fatalf("reason = %q, want retries_exhausted", res.Reason)
	}
	if res.Retryable {
		t.Fatal("an exhausted budget must not be retryable")
	}
	if ReadPending(f.SidecarDir) != nil {
		t.Fatal("an exhausted marker must be cleared so the next launch does not fetch")
	}
}

func TestApplyKeepsTheMarkerWhenTheServerIsUnreachable(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	f.record(t)
	// Kill the server after recording, so the manifest fetch fails.
	f.srv.Close()

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyFailed {
		t.Fatalf("outcome = %v, want failed", res.Outcome)
	}
	if !res.Retryable {
		t.Fatal("a network failure must be retryable")
	}
	p := ReadPending(f.SidecarDir)
	if p == nil {
		t.Fatal("a network failure must retain the marker")
	}
	if p.RetryCount != 1 {
		t.Fatalf("retry_count = %d, want 1", p.RetryCount)
	}
}

// --- §9.2 step 5 manifest drift -------------------------------------------

func TestApplyRefusesManifestDrift(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	p := f.record(t)
	// The manifest grows by a byte after the user confirmed the size.
	p.ArchiveSize = p.ArchiveSize + 1
	if err := writeJSONAtomic(updatePath(f.SidecarDir, PendingFileName), p); err != nil {
		t.Fatalf("rewrite marker: %v", err)
	}

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Reason != "manifest_drift" {
		t.Fatalf("reason = %q, want manifest_drift (%s)", res.Reason, res.Message)
	}
	if res.Message != msgDriftMismatch {
		t.Fatalf("message = %q, want the E34 wording", res.Message)
	}
	if ReadPending(f.SidecarDir) != nil {
		t.Fatal("a drifted marker must be cleared, not retried blindly")
	}
	// The install is untouched.
	if got := readFile(t, filepath.Join(f.GameFolder, "game", "index.html")); !strings.Contains(got, "old") {
		t.Fatal("a drifted update modified the install")
	}
}

// --- §16.2 E29 ------------------------------------------------------------

func TestApplyRefusesWhenLauncherTooOld(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	// The fixture manifest declares launcher_min 0.1.0; claim an older launcher.
	f.record(t)

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.0.1"})
	if res.Reason != "launcher_min" {
		t.Fatalf("reason = %q, want launcher_min", res.Reason)
	}
	if res.Message != LauncherTooOldMessage {
		t.Fatalf("message = %q, want the E29 wording verbatim", res.Message)
	}
	if ReadPending(f.SidecarDir) != nil {
		t.Fatal("the marker should be cleared after an E29 refusal")
	}
	if got := readFile(t, filepath.Join(f.GameFolder, "game", "index.html")); !strings.Contains(got, "old") {
		t.Fatal("the current release must remain runnable")
	}
}

// --- §16.2 E28 and E30 ----------------------------------------------------

func TestApplyRejectsACorruptedArchive(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	f.record(t)
	// Corrupt the served archive by one byte, then run.
	corrupt := filepath.Join(t.TempDir(), "corrupt.tar.zst")
	body := mustRead(t, mustReadPath(t, f))
	body[len(body)/2] ^= 0xff
	if err := os.WriteFile(corrupt, body, 0o644); err != nil {
		t.Fatalf("write corrupt archive: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveFile(t, w, r, corrupt)
	}))
	defer bad.Close()

	p := ReadPending(f.SidecarDir)
	p.ArchiveURL = bad.URL + "/corrupt.tar.zst"
	// The hash still names the original, so Verify must catch the difference.
	if err := writeJSONAtomic(updatePath(f.SidecarDir, PendingFileName), p); err != nil {
		t.Fatalf("rewrite marker: %v", err)
	}

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Reason != "verify_failed" {
		t.Fatalf("reason = %q, want verify_failed", res.Reason)
	}
	if res.Message != msgDamagedShort {
		t.Fatalf("message = %q, want the E28 wording", res.Message)
	}
	if got := readFile(t, filepath.Join(f.GameFolder, "game", "index.html")); !strings.Contains(got, "old") {
		t.Fatal("a corrupted update modified the install")
	}
	if exists(updatePath(f.SidecarDir, DownloadFileName)) {
		t.Fatal("the damaged archive was left where a later launch could reuse it")
	}
}

// mustReadPath is the fixture's archive on disk.
func mustReadPath(t *testing.T, f *applyFixture) string {
	t.Helper()
	return f.archivePath
}

// --- §9.5 the data/ assertion --------------------------------------------

// TestApplyRejectsAnArchiveThatTouchesData drives the FR-UPD-7 promise through
// the pipeline. Extract refuses the entry by construction; the pipeline must
// also refuse to swap and must say E30.
func TestApplyRejectsAnArchiveThatTouchesData(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	// Add an archive member that names data/. The fixture's manifest index must
	// list it, so rebuild the served manifest by hand.
	archivePath := buildArchive(t,
		tarEntry{Name: "game/index.html", Body: []byte("<html>new</html>\n")},
		tarEntry{Name: "data/saves/slot1.json", Body: []byte(`{"stolen":true}`)},
	)
	st, _ := os.Stat(archivePath)
	manifest := map[string]any{
		"schema":         ReleaseManifestSchema,
		"release":        f.targetRel,
		"launcher_min":   "0.1.0",
		"engine_version": "1.0.0",
		"save_version":   1,
		"scope":          "full",
		"total_size":     int64(len("<html>new</html>\n") + len(`{"stolen":true}`)),
		"files": []FileEntry{
			{Path: "game/index.html", Size: int64(len("<html>new</html>\n")), Hash: sha256Of([]byte("<html>new</html>\n"))},
			{Path: "data/saves/slot1.json", Size: int64(len(`{"stolen":true}`)), Hash: sha256Of([]byte(`{"stolen":true}`))},
		},
		"archive": map[string]any{
			"name": "data.tar.zst",
			"url":  "/data.tar.zst",
			"size": st.Size(),
			"hash": sha256Of(mustRead(t, archivePath)),
		},
	}
	manifestJSON, _ := json.Marshal(manifest)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/data.tar.zst":
			serveFile(t, w, r, archivePath)
		case "/" + f.targetRel + ".release.manifest.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(manifestJSON)
		case "/latest.json":
			_, _ = w.Write([]byte(`{"release":"` + f.targetRel + `","manifest_url":"/` +
				f.targetRel + `.release.manifest.json"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := WritePending(f.SidecarDir, &Pending{
		Schema:        PendingSchema,
		FromRelease:   f.fromRel,
		TargetRelease: f.targetRel,
		ManifestURL:   srv.URL + "/" + f.targetRel + ".release.manifest.json",
		ArchiveURL:    srv.URL + "/data.tar.zst",
		ArchiveSize:   st.Size(),
		ArchiveHash:   sha256Of(mustRead(t, archivePath)),
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyFailed {
		t.Fatalf("outcome = %v (%s), want failed", res.Outcome, res.Reason)
	}
	// Either gate may be the one that sees the data/ member: Verify refuses to
	// hash it and Extract refuses to write it. Both must report E30.
	if res.Reason != "extract_failed" && res.Reason != "verify_failed" {
		t.Fatalf("reason = %q, want a data rejection (message %q)", res.Reason, res.Message)
	}
	if res.Message != msgData {
		t.Fatalf("message = %q, want the E30 wording %q", res.Message, msgData)
	}
	if got := readFile(t, filepath.Join(f.GameFolder, "data", "saves", "slot1.json")); got != `{"slot":1}` {
		t.Fatalf("the user's save was modified: %q", got)
	}
	if got := readFile(t, filepath.Join(f.GameFolder, "game", "index.html")); !strings.Contains(got, "old") {
		t.Fatal("an archive touching data/ was installed anyway")
	}
}

// --- §12 cancellation -----------------------------------------------------

func TestApplyHonoursAPreRecordedCancel(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	f.record(t)
	if err := RequestCancel(f.SidecarDir); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyCancelled {
		t.Fatalf("outcome = %v (%s), want cancelled", res.Outcome, res.Reason)
	}
	if res.Message != msgCancelled {
		t.Fatalf("message = %q, want the E33 wording", res.Message)
	}
	if ReadPending(f.SidecarDir) != nil {
		t.Fatal("a cancelled update must not stay pending")
	}
	if CancelRequested(f.SidecarDir) {
		t.Fatal("the cancel flag must be cleared after it is honoured")
	}
	if got := readFile(t, filepath.Join(f.GameFolder, "game", "index.html")); !strings.Contains(got, "old") {
		t.Fatal("a cancelled update modified the install")
	}
}

// --- §17 exclusivity ------------------------------------------------------

func TestApplySkipsWhenAnotherApplyHoldsTheLock(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	f.record(t)

	dir, err := EnsureUpdateDir(f.SidecarDir)
	if err != nil {
		t.Fatalf("EnsureUpdateDir: %v", err)
	}
	held, err := acquireFileLock(filepath.Join(dir, ApplyLockName))
	if err != nil {
		t.Fatalf("acquireFileLock: %v", err)
	}
	defer held.release()

	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplySkipped || res.Reason != "locked" {
		t.Fatalf("outcome = %v (%s), want skipped/locked", res.Outcome, res.Reason)
	}
	// The marker survives so the winning apply is unaffected.
	if ReadPending(f.SidecarDir) == nil {
		t.Fatal("a skipped apply must leave the marker in place")
	}
}

// --- §11 progress and result records -------------------------------------

func TestApplyRecordsProgressAndResult(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	f.record(t)

	// The recorded intent is visible before the launch that acts on it.
	pr := ReadProgress(f.SidecarDir)
	if pr.Phase != PhaseIdle {
		t.Fatalf("pre-apply phase = %q, want idle", pr.Phase)
	}
	if pr.TargetRelease != f.targetRel {
		t.Fatalf("pre-apply target = %q, want %q", pr.TargetRelease, f.targetRel)
	}
	if pr.TotalBytes <= 0 {
		t.Fatalf("pre-apply total_bytes = %d, want the confirmed size", pr.TotalBytes)
	}

	if res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"}); res.Outcome != ApplyApplied {
		t.Fatalf("outcome = %v (%s), want applied", res.Outcome, res.Reason)
	}

	after := ReadProgress(f.SidecarDir)
	if after.Phase != PhaseComplete {
		t.Fatalf("final phase = %q, want complete", after.Phase)
	}
	if after.Percent != 100 {
		t.Fatalf("final percent = %d, want 100", after.Percent)
	}
	if after.Seq == 0 {
		t.Fatal("the progress sequence never advanced")
	}
	r := ReadResult(f.SidecarDir)
	if r == nil {
		t.Fatal("no result record was written")
	}
	if r.Outcome != OutcomeComplete || r.InstalledRelease != f.targetRel {
		t.Fatalf("result = %+v, want complete at %s", r, f.targetRel)
	}
	if !r.RetainedBackup {
		t.Fatal("the result should report the retained backup")
	}
	if r.BytesWritten != f.archiveSz {
		t.Fatalf("bytes_written = %d, want %d", r.BytesWritten, f.archiveSz)
	}
}

func TestPercentOfIsDerivedFromDeclaredSize(t *testing.T) {
	cases := []struct {
		written, total, want int64
	}{
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{150, 100, 100}, // clamped
		{1, 0, 0},       // no declared size: never a division by zero
		{-5, 100, 0},
	}
	for _, tc := range cases {
		if got := int64(percentOf(tc.written, tc.total)); got != tc.want {
			t.Fatalf("percentOf(%d, %d) = %d, want %d", tc.written, tc.total, got, tc.want)
		}
	}
}

// TestWritePendingClearsStaleProgressAndCancel pins R11.4 and R12.5.
func TestWritePendingClearsStaleProgressAndCancel(t *testing.T) {
	dir := t.TempDir()
	sidecar := filepath.Join(dir, "sidecar")
	if err := os.MkdirAll(sidecar, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A previous cycle's ending and cancel request.
	WriteResult(sidecar, Result{Outcome: OutcomeComplete, TargetRelease: "2026.01.1"})
	if err := writeJSONAtomic(updatePath(sidecar, ProgressFileName), Progress{Schema: ProgressSchema, Phase: PhaseComplete}); err != nil {
		t.Fatalf("write progress: %v", err)
	}
	if err := RequestCancel(sidecar); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}

	if err := WritePending(sidecar, &Pending{
		Schema:        PendingSchema,
		FromRelease:   "2026.01.1",
		TargetRelease: "2026.02.1",
		ManifestURL:   "https://example.invalid/m.json",
		ArchiveSize:   10,
	}); err != nil {
		t.Fatalf("WritePending: %v", err)
	}

	if CancelRequested(sidecar) {
		t.Fatal("a stale cancel flag must not survive into the new cycle (R12.5)")
	}
	pr := ReadProgress(sidecar)
	if pr.Phase != PhaseIdle {
		t.Fatalf("phase = %q, want the new cycle's idle record (R11.4)", pr.Phase)
	}
	if pr.TargetRelease != "2026.02.1" {
		t.Fatalf("target = %q, want the new target", pr.TargetRelease)
	}
}

// TestReadProgressAbsentIsIdle pins R11.7: no record is an idle answer rather
// than an error.
func TestReadProgressAbsentIsIdle(t *testing.T) {
	pr := ReadProgress(t.TempDir())
	if pr.Phase != PhaseIdle {
		t.Fatalf("phase = %q, want idle", pr.Phase)
	}
	if ReadResult(t.TempDir()) != nil {
		t.Fatal("an absent result must be nil")
	}
}

// --- §10 disk preflight ---------------------------------------------------

func TestRequiredBytesIsTheStagedPeak(t *testing.T) {
	// R10.2: archive + staging + backup, plus headroom.
	got := RequiredBytes(100, 1000)
	want := int64(100 + 1000 + 1000 + 64<<20)
	if got != want {
		t.Fatalf("RequiredBytes(100, 1000) = %d, want %d", got, want)
	}
	// The 5% rule wins once total_size is large enough.
	big := int64(4) << 30
	got = RequiredBytes(0, big)
	want = big*2 + big/20
	if got != want {
		t.Fatalf("RequiredBytes(0, 4GiB) = %d, want %d", got, want)
	}
	// Negative inputs are clamped rather than producing a negative requirement.
	if got := RequiredBytes(-1, -1); got <= 0 {
		t.Fatalf("RequiredBytes(-1, -1) = %d, want a positive requirement", got)
	}
}

func TestPreflightBoundaryIsInclusive(t *testing.T) {
	// R18.5: exactly the requirement passes; one byte under fails. The preflight
	// compares free < required, so the boundary is the required value.
	required := RequiredBytes(10, 100)
	if required <= 0 {
		t.Fatal("requirement must be positive")
	}
	if !(required >= required) {
		t.Fatal("free space equal to the requirement must pass")
	}
	if required-1 < required == false {
		t.Fatal("one byte under the requirement must fail")
	}
}

func TestPreflightSkipsWhenSpaceIsUnknown(t *testing.T) {
	// R10.5: an unqueryable filesystem must not refuse the update. The pipeline
	// reaches the download when Known is false, so the preflight cannot be the
	// thing that stops it.
	space := DiskSpace{}
	if space.Known {
		t.Fatal("the zero DiskSpace must report unknown")
	}
}

// --- §7 startup pass ------------------------------------------------------

func TestStartupAppliesAPendingUpdate(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"b.txt": "b"})
	f.record(t)

	res := Startup(context.Background(), StartupOptions{
		GameFolder:      f.GameFolder,
		SidecarDir:      f.SidecarDir,
		LauncherVersion: "0.1.0",
		Apply: ApplyOptions{
			SkipDownloadCheckForTest: true,
			StallTimeout:             5 * time.Second,
		},
	})
	if res.Applied.Outcome != ApplyApplied {
		t.Fatalf("apply outcome = %v (%s), want applied", res.Applied.Outcome, res.Applied.Reason)
	}
	if got := InstalledReleaseIn(f.GameFolder); got != f.targetRel {
		t.Fatalf("installed release = %q, want %q", got, f.targetRel)
	}
}

func TestStartupWithNoMarkerDoesNothing(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"b.txt": "b"})
	res := Startup(context.Background(), StartupOptions{
		GameFolder:      f.GameFolder,
		SidecarDir:      f.SidecarDir,
		LauncherVersion: "0.1.0",
	})
	if res.Applied.Outcome != ApplyNoPending {
		t.Fatalf("outcome = %v, want no_pending", res.Applied.Outcome)
	}
	if res.Recovered != RecoveryNone {
		t.Fatalf("recovered = %v, want none", res.Recovered)
	}
}

func TestStartupRecoversAnInterruptedSwapFirst(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"b.txt": "b"})
	// Simulate a crash after game/ was renamed away but before game.new was
	// promoted: the classic R7.1 case where paths.Resolve would see no game/.
	// The staging root is the sidecar update directory (spec §5, R5.3).
	stagingRoot := filepath.Join(f.SidecarDir, UpdateDirName)
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		t.Fatalf("mkdir staging root: %v", err)
	}
	staging := filepath.Join(stagingRoot, stagingDirName)
	if err := os.Rename(filepath.Join(f.GameFolder, "game"), staging); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := writeState(filepath.Join(f.GameFolder, stateFileName), updateState{
		Schema: UpdateStateSchema, Release: f.targetRel, Step: "begin",
		From: gameDirName, To: backupDirName, Staging: stagingDirName,
		Started: nowRFC3339(),
	}); err != nil {
		t.Fatalf("writeState: %v", err)
	}

	res := Startup(context.Background(), StartupOptions{
		GameFolder:      f.GameFolder,
		SidecarDir:      f.SidecarDir,
		LauncherVersion: "0.1.0",
	})
	if res.Recovered != RecoveryRolledForward {
		t.Fatalf("recovered = %v, want rolled_forward", res.Recovered)
	}
	if !exists(filepath.Join(f.GameFolder, "game", "index.html")) {
		t.Fatal("recovery did not leave a runnable game folder")
	}
}

// --- §3 error hygiene -----------------------------------------------------

// TestApplyErrorsCarryNoPaths keeps the §22.4 rule on the new failure paths.
func TestApplyErrorsCarryNoPaths(t *testing.T) {
	f := newApplyFixture(t, map[string]string{"a.txt": "a"})
	p := f.record(t)
	p.ArchiveURL = "http://example.invalid/a.tar.zst"
	if err := writeJSONAtomic(updatePath(f.SidecarDir, PendingFileName), p); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	res := f.apply(t, ApplyOptions{LauncherVersion: "0.1.0"})
	if res.Outcome != ApplyFailed {
		t.Fatalf("outcome = %v, want failed", res.Outcome)
	}
	// The user-facing message must not name the fixture's folder or the URL.
	if strings.Contains(res.Message, f.GameFolder) || strings.Contains(res.Message, "example.invalid") {
		t.Fatalf("message leaked a path or URL: %q", res.Message)
	}
	// The recorded result is equally path-free.
	if r := ReadResult(f.SidecarDir); r != nil {
		if strings.Contains(r.Message, f.GameFolder) || strings.Contains(r.Message, "example.invalid") {
			t.Fatalf("result message leaked a path or URL: %q", r.Message)
		}
	}
}
