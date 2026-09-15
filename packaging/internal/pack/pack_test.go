package pack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// --- schema relaxation (Packaging spec §12 A8) -----------------------------

func TestStripLookaheadsRemovesOnlyLookahead(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		removed int
	}{
		{`^(game|launcher)/(?!\.\.?(/|$))[A-Za-z0-9._-]+$`, `^(game|launcher)/[A-Za-z0-9._-]+$`, 1},
		{`^(?!\.\.?(/|$))a(/(?!\.\.?(/|$))b)*$`, `^a(/b)*$`, 2},
		{`^[a-z]+$`, `^[a-z]+$`, 0},
		// A nested group inside the lookahead must be skipped whole.
		{`^x(?!(a(b)c))y$`, `^xy$`, 1},
	}
	for _, tc := range cases {
		got, removed := stripLookaheads(tc.in)
		if got != tc.want || removed != tc.removed {
			t.Errorf("stripLookaheads(%q) = (%q, %d), want (%q, %d)",
				tc.in, got, removed, tc.want, tc.removed)
		}
	}
}

func TestRelaxedSchemasStillCompile(t *testing.T) {
	log := NewLogger(io.Discard, false)
	validator, err := NewValidator(log)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	if len(validator.compiled) < 6 {
		t.Fatalf("compiled %d schemas, want at least 6", len(validator.compiled))
	}
}

// --- the code half of §12 A8 ----------------------------------------------

func TestCheckRelPath(t *testing.T) {
	ok := []string{"game/index.html", "game/assets/a/b.png", "launcher/launcher", "launcher/VERSION"}
	for _, p := range ok {
		if err := CheckRelPath(p, RelPathPrefixes); err != nil {
			t.Errorf("CheckRelPath(%q) = %v, want nil", p, err)
		}
	}
	bad := map[string]string{
		"data/saves/slot1.json":   "prefix",
		"game/../launcher/launch": "dot segment",
		"/etc/passwd":             "absolute",
		`game\index.html`:         "backslash",
		"game/./index.html":       "dot segment",
		"game/index.html/":        "empty segment",
		"C:/game/index.html":      "drive",
		"":                        "empty",
		"game":                    "prefix",
	}
	for p, why := range bad {
		if err := CheckRelPath(p, RelPathPrefixes); err == nil {
			t.Errorf("CheckRelPath(%q) = nil, want an error (%s)", p, why)
		}
	}
}

func TestCheckFileIndexRejectsData(t *testing.T) {
	if err := CheckFileIndex([]string{"game/index.html", "data/saves/slot1.json"}); err == nil {
		t.Fatal("CheckFileIndex accepted a data/ path (FR-UPD-7)")
	}
	if err := CheckFileIndex([]string{"game/index.html", "launcher/launcher"}); err != nil {
		t.Fatalf("CheckFileIndex rejected a valid index: %v", err)
	}
}

// --- config ----------------------------------------------------------------

func TestConfigRejectsSlugThatDisagreesWithGameID(t *testing.T) {
	cfg := &Config{
		Package: PackageConfig{Slug: "wrong", GameID: "com.kobra.right", GameName: "X", PkgRoot: "."},
		Release: ReleaseConfig{
			Release: "2026.09.1", GameVersion: "1.0.0", EngineVersion: "0.1.0",
			SaveVersion: 1, LauncherMin: "0.1.0",
		},
		Platforms: map[string]PlatformConfig{"linux-x64": {GOOS: "linux", GOARCH: "amd64", Binary: "launcher", ArchivePlatform: "linux-x64"}},
		Dir:       t.TempDir(),
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a slug that is not the last segment of game_id")
	}
	if !strings.Contains(err.Error(), "slug") {
		t.Fatalf("error does not name the slug rule: %v", err)
	}
}

// TestConfigRejectsUnparseableLauncherMin pins the publisher-side half of the
// E37 rule.
//
// The schema requires launcher_min to match ^[0-9]+\.[0-9]+\.[0-9]+$, so a
// conforming pipeline cannot produce a manifest whose floor the launcher is
// unable to read — which is why Packaging spec §9.5 treats a manifest in the wild
// that fails this as proof it came from a non-conforming build. Version fields in
// this project are bare X.Y.Z: pre-release status is the release manifest's
// `channel`, not a `-rc.1` suffix (VERSIONING.md).
func TestConfigRejectsUnparseableLauncherMin(t *testing.T) {
	for _, bad := range []string{"", "1.0.0-rc.1", "1.0", "v1.0.0", "1.0.0+build", "latest"} {
		cfg := &Config{
			Package: PackageConfig{Slug: "right", GameID: "com.kobra.right", GameName: "X", PkgRoot: "."},
			Release: ReleaseConfig{
				Release: "2026.09.1", GameVersion: "1.0.0", EngineVersion: "0.1.0",
				SaveVersion: 1, LauncherMin: bad,
			},
			Platforms: map[string]PlatformConfig{"linux-x64": {GOOS: "linux", GOARCH: "amd64", Binary: "launcher", ArchivePlatform: "linux-x64"}},
			Dir:       t.TempDir(),
		}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("Validate accepted launcher_min %q, which no launcher can read", bad)
		}
		if !strings.Contains(err.Error(), "launcher_min") {
			t.Fatalf("launcher_min %q: error does not name the field: %v", bad, err)
		}
	}
}

// --- the pipeline, hermetically --------------------------------------------

// fixture builds a minimal but complete pkgroot in a temp directory, with a
// stub launcher binary that answers --version. It lets the archive and manifest
// rules be tested without the real launcher or the testgame fixture.
func fixture(t *testing.T) (configPath string, launcher string) {
	t.Helper()
	root := t.TempDir()

	write := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("game/index.html", "<!doctype html><title>t</title>")
	write("game/shell.js", "// shell\n")
	write("game/engine/game.wasm", "\x00asm\x01\x00\x00\x00")
	write("game/engine/game.js", "// glue\n")
	write("game/engine/engine.manifest.json", `{
  "schema": "kobra.engine-manifest/1",
  "release": "2026.09.1",
  "engine_version": "0.1.0",
  "wasm": "engine/game.wasm",
  "glue": "engine/game.js",
  "save_version": 1
}`)
	write("game/assets/data/levels.json", `{"levels":[]}`)
	write("launcher/launcher.config.json", `{
  "schema": "kobra.launcher-config/1",
  "game_id": "com.kobra.fixture",
  "game_name": "Fixture",
  "release": "2026.09.1",
  "port": { "base": 18900, "span": 50, "require_confirmation": false }
}`)

	// A stub launcher: the pipeline only ever runs it with --version.
	launcher = filepath.Join(root, "stublauncher")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\necho \"kobra-launcher 0.1.0 (stub) built now\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	configPath = filepath.Join(root, "pkg.toml")
	if err := os.WriteFile(configPath, []byte(`
[package]
slug = "fixture"
game_id = "com.kobra.fixture"
game_name = "Fixture"
pkgroot = "."

[release]
release = "2026.09.1"
game_version = "1.0.0"
engine_version = "0.1.0"
save_version = 1
launcher_min = "0.1.0"
channel = "stable"
scope = "full"

[launcher]
config = "launcher/launcher.config.json"
deny_list = "deny.json"

[platforms.linux-x64]
goos = "linux"
goarch = "amd64"
binary = "launcher"
archive_platform = "linux-x64"

[publish]
base = "https://example.invalid/fixture"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deny.json"), []byte(`{"ports":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath, launcher
}

func newFixturePipeline(t *testing.T) (*Pipeline, string) {
	t.Helper()
	configPath, launcher := fixture(t)
	log := NewLogger(io.Discard, false)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pipeline, err := NewPipeline(cfg, log)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pipeline, launcher
}

func TestPipelineBuildsAndVerifies(t *testing.T) {
	pipeline, launcher := newFixturePipeline(t)
	out := t.TempDir()
	opts := Options{Platform: "linux-x64", Launcher: launcher, OutDir: out}

	if err := pipeline.Check(opts); err != nil {
		t.Fatalf("Check: %v", err)
	}
	// Check must write nothing.
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Fatalf("Check wrote %d entries into the output directory", len(entries))
	}

	result, err := pipeline.Run(opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Releases) != 1 {
		t.Fatalf("built %d releases, want 1", len(result.Releases))
	}

	name := "fixture-2026.09.1-linux-x64"
	// The update archive must not carry the manifest (§1.2, M1).
	members := tarMembers(t, filepath.Join(out, name+".tar.zst"))
	if _, ok := members[ManifestRel]; ok {
		t.Fatalf("the update archive contains %s", ManifestRel)
	}
	if len(members) == 0 {
		t.Fatal("the update archive is empty")
	}

	// The ZIP must carry it, byte-identical to the published copy.
	zipBytes := zipEntry(t, filepath.Join(out, name+".zip"), "Fixture/"+ManifestRel)
	published, err := os.ReadFile(filepath.Join(out, "fixture-2026.09.1.release.manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(zipBytes, published) {
		t.Fatal("the ZIP's manifest differs from the published manifest")
	}

	// Every archive member must be in the index the launcher will check.
	var manifest ReleaseManifest
	if err := ReadJSON(filepath.Join(out, "fixture-2026.09.1.release.manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	indexed := map[string]bool{}
	for _, f := range manifest.Files {
		indexed[f.Path] = true
	}
	for member := range members {
		if !indexed[member] {
			t.Errorf("archive member %q is not in the manifest index; the launcher rejects that", member)
		}
	}

	// §8.2.2: the manifest names the archive, and the hash is right.
	if manifest.Archive == nil || manifest.Archive.Hash == "" {
		t.Fatal("the manifest does not declare its archive")
	}
	hash, size, err := HashFile(filepath.Join(out, name+".tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Archive.Hash != hash || manifest.Archive.Size != size {
		t.Fatalf("archive identity mismatch: manifest %s/%d, actual %s/%d",
			manifest.Archive.Hash, manifest.Archive.Size, hash, size)
	}

	// The final gate the build runs must pass on its own output.
	if err := VerifyDirectory(out, NewLogger(io.Discard, false)); err != nil {
		t.Fatalf("VerifyDirectory: %v", err)
	}
}

func TestPipelineIsDeterministic(t *testing.T) {
	pipeline, launcher := newFixturePipeline(t)
	out := t.TempDir()
	opts := Options{Platform: "linux-x64", Launcher: launcher, OutDir: out}

	if _, err := pipeline.Run(opts); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	first := hashDir(t, out)
	if _, err := pipeline.Run(opts); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	second := hashDir(t, out)

	if len(first) != len(second) {
		t.Fatalf("run produced %d then %d files", len(first), len(second))
	}
	for name, hash := range first {
		if second[name] != hash {
			t.Errorf("%s changed between identical builds (%s -> %s)", name, hash, second[name])
		}
	}
}

func TestPipelineRejectsUnservableRootFile(t *testing.T) {
	pipeline, launcher := newFixturePipeline(t)
	// A stylesheet at the game root is packaged, indexed and then 404s: the
	// launcher only serves index.html and shell.js from there.
	root := pipeline.cfg.Dir
	if err := os.WriteFile(filepath.Join(root, "game", "extra.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := pipeline.Check(Options{Platform: "linux-x64", Launcher: launcher})
	if err == nil {
		t.Fatal("Check accepted an unservable file at the game root")
	}
	if !strings.Contains(err.Error(), "game/extra.css") {
		t.Fatalf("error does not name the offending path: %v", err)
	}
}

func TestPipelineRejectsIdentityDisagreement(t *testing.T) {
	pipeline, launcher := newFixturePipeline(t)
	// §4.6: the engine manifest's save_version must equal the release's.
	path := filepath.Join(pipeline.cfg.Dir, "game", "engine", "engine.manifest.json")
	doc := `{"schema":"kobra.engine-manifest/1","release":"2026.09.1","engine_version":"0.1.0",` +
		`"wasm":"engine/game.wasm","glue":"engine/game.js","save_version":2}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	err := pipeline.Check(Options{Platform: "linux-x64", Launcher: launcher})
	if err == nil {
		t.Fatal("Check accepted a save_version disagreement")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Fatalf("error is not classified as an identity failure: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

func tarMembers(t *testing.T, archive string) map[string]int64 {
	t.Helper()
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	plain, err := decoder.DecodeAll(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	reader := tar.NewReader(bytes.NewReader(plain))
	for {
		hdr, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = hdr.Size
	}
	return out
}

func zipEntry(t *testing.T, archive, name string) []byte {
	t.Helper()
	reader, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, f := range reader.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		raw, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	t.Fatalf("%s is not in %s", name, archive)
	return nil
}

func hashDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		hash, _, err := HashFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = hash
	}
	return out
}

// TestEmbeddedSchemasMatchArchitecture keeps the vendored copy honest. It is the
// reason a schema change cannot silently fail to reach the tool.
//
// It checks both directions. A stale file under packaging/internal/pack/schemas
// is drift, and so is a newly published schema that never arrived here — the
// first is caught by comparing what is embedded, the second only by walking the
// published set. `make sync-schemas` is what repairs either.
func TestEmbeddedSchemasMatchArchitecture(t *testing.T) {
	archDir := filepath.Join("..", "..", "..", "architecture", "schemas")
	if _, err := os.Stat(archDir); err != nil {
		t.Skip("architecture/schemas is not present in this checkout")
	}
	entries, err := schemaFS.ReadDir("schemas")
	if err != nil {
		t.Fatal(err)
	}
	embedded := map[string]bool{}
	for _, entry := range entries {
		embedded[entry.Name()] = true
		got, err := schemaFS.ReadFile("schemas/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(archDir, entry.Name()))
		if err != nil {
			t.Errorf("%s is embedded but missing from architecture/schemas; run `make sync-schemas` at the repository root to remove it", entry.Name())
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from architecture/schemas/%s; run `make sync-schemas` at the repository root", entry.Name(), entry.Name())
		}
	}

	archEntries, err := os.ReadDir(archDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range archEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if !embedded[entry.Name()] {
			t.Errorf("architecture/schemas/%s is not embedded in packaging/internal/pack/schemas; run `make sync-schemas` at the repository root", entry.Name())
		}
	}
}

// --- FIX 1: `check` is the same gate as `build` -----------------------------

// TestCheckAndBuildAgree is the regression for the whole-tree gate that used to
// live only in Run. `check` and `build` must reach the same verdict on the same
// tree, including the release-manifest path pattern and the required root files.
func TestCheckAndBuildAgree(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, root string)
		wantErr bool
	}{
		{
			name:   "good tree",
			mutate: func(t *testing.T, root string) {},
		},
		{
			name: "pipe in a filename",
			mutate: func(t *testing.T, root string) {
				writeFixtureFile(t, filepath.Join(root, "game", "engine", "a|b.js"), "x")
			},
			wantErr: true,
		},
		{
			name: "missing required root file",
			mutate: func(t *testing.T, root string) {
				if err := os.Remove(filepath.Join(root, "game", "index.html")); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pipeline, launcher := newFixturePipeline(t)
			tc.mutate(t, pipeline.cfg.Dir)

			checkErr := pipeline.Check(Options{Platform: "linux-x64", Launcher: launcher})
			_, runErr := pipeline.Run(Options{
				Platform: "linux-x64", Launcher: launcher, OutDir: t.TempDir(),
			})
			if (checkErr == nil) != (runErr == nil) {
				t.Fatalf("check and build disagree: check=%v build=%v", checkErr, runErr)
			}
			if tc.wantErr && checkErr == nil {
				t.Fatal("check accepted a tree the build must reject")
			}
			if !tc.wantErr && runErr != nil {
				t.Fatalf("build rejected a good tree: %v", runErr)
			}
		})
	}
}

// TestCheckRejectsUnsafeFileNameInIndex names the specific gate: the release
// manifest's files[].path pattern, which only Run used to apply.
func TestCheckRejectsUnsafeFileNameInIndex(t *testing.T) {
	pipeline, launcher := newFixturePipeline(t)
	writeFixtureFile(t, filepath.Join(pipeline.cfg.Dir, "game", "engine", "a|b.js"), "x")

	err := pipeline.Check(Options{Platform: "linux-x64", Launcher: launcher})
	if err == nil {
		t.Fatal("Check accepted a path the release-manifest schema forbids")
	}
	if !strings.Contains(err.Error(), "schema") {
		t.Fatalf("error is not classified as a schema failure: %v", err)
	}
}

// --- FIX 2: required root files --------------------------------------------

// TestRequiredRootFilesAreEnforced covers §5.4 and §11.2 gate 1. Absence used to
// be invisible because CheckServable only inspected files that were present.
func TestRequiredRootFilesAreEnforced(t *testing.T) {
	for _, required := range []string{"index.html", "shell.js"} {
		t.Run(required, func(t *testing.T) {
			pipeline, launcher := newFixturePipeline(t)
			if err := os.Remove(filepath.Join(pipeline.cfg.Dir, "game", required)); err != nil {
				t.Fatal(err)
			}

			checkErr := pipeline.Check(Options{Platform: "linux-x64", Launcher: launcher})
			if checkErr == nil {
				t.Fatalf("Check accepted a tree with no game/%s", required)
			}
			if !strings.Contains(checkErr.Error(), "game/"+required) {
				t.Fatalf("error does not name the missing file: %v", checkErr)
			}
			if strings.Contains(checkErr.Error(), pipeline.cfg.Dir) {
				t.Fatalf("error leaks a filesystem path: %v", checkErr)
			}

			if _, runErr := pipeline.Run(Options{
				Platform: "linux-x64", Launcher: launcher, OutDir: t.TempDir(),
			}); runErr == nil {
				t.Fatalf("build accepted a tree with no game/%s", required)
			}
		})
	}
}

// --- FIX 3: `verify` checks the manifest index against the bytes ------------

// TestVerifyRejectsTamperedZipMember reproduces the review's repro: replace one
// member of the ZIP, regenerate SHA256SUMS, and `verify` must still fail because
// the release manifest's files[].hash no longer matches the archived bytes.
func TestVerifyRejectsTamperedZipMember(t *testing.T) {
	out := buildFixtureDist(t)

	zipPath := filepath.Join(out, "fixture-2026.09.1-linux-x64.zip")
	member := zipTopDir(t, zipPath) + "/game/index.html"
	original := zipEntry(t, zipPath, member)
	tampered := append([]byte(nil), original...)
	tampered[0] ^= 0xff // same length, so only the hash can catch it
	rewriteZipMember(t, zipPath, member, tampered)
	regenerateSums(t, out)

	err := VerifyDirectory(out, NewLogger(io.Discard, false))
	if err == nil {
		t.Fatal("verify accepted a ZIP whose member no longer matches the manifest index")
	}
	if !strings.Contains(err.Error(), "index") {
		t.Fatalf("error does not name the index mismatch: %v", err)
	}
}

// TestVerifyRejectsManifestIndexMismatch tampers the published manifest itself:
// the archive is untouched and SHA256SUMS is regenerated, so only the per-file
// index check against the archived bytes can catch it.
func TestVerifyRejectsManifestIndexMismatch(t *testing.T) {
	out := buildFixtureDist(t)

	manifestPath := filepath.Join(out, "fixture-2026.09.1.release.manifest.json")
	var manifest ReleaseManifest
	if err := ReadJSON(manifestPath, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("the fixture manifest has no files")
	}
	manifest.Files[0].Hash = "sha256:" + strings.Repeat("0", 64)
	if err := WriteJSON(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	regenerateSums(t, out)

	if err := VerifyDirectory(out, NewLogger(io.Discard, false)); err == nil {
		t.Fatal("verify accepted a manifest whose files[].hash does not match the archive")
	}
}

// --- FIX 4: `verify` rejects an incomplete dist ----------------------------

func TestVerifyRejectsMissingReleaseManifests(t *testing.T) {
	out := buildFixtureDist(t)
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".release.manifest.json") {
			if err := os.Remove(filepath.Join(out, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	regenerateSums(t, out)

	err = VerifyDirectory(out, NewLogger(io.Discard, false))
	if err == nil {
		t.Fatal("verify accepted a dist whose archives have no release manifest")
	}
	if !strings.Contains(err.Error(), "release manifest") {
		t.Fatalf("error does not name the missing manifest: %v", err)
	}
}

func TestVerifyRejectsDanglingLatest(t *testing.T) {
	cases := []struct {
		name  string
		write func(t *testing.T, out string)
	}{
		{
			name: "latest names a missing manifest",
			write: func(t *testing.T, out string) {
				if err := WriteLatest(out, Latest{
					Release:     "2026.09.1",
					ManifestURL: "gone.release.manifest.json",
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "latest names a hash-mismatched manifest",
			write: func(t *testing.T, out string) {
				manifestPath := filepath.Join(out, "fixture-2026.09.1.release.manifest.json")
				var manifest ReleaseManifest
				if err := ReadJSON(manifestPath, &manifest); err != nil {
					t.Fatal(err)
				}
				manifest.Archive.Hash = "sha256:" + strings.Repeat("0", 64)
				if err := WriteJSON(manifestPath, manifest); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := buildFixtureDist(t)
			tc.write(t, out)
			regenerateSums(t, out)
			if err := VerifyDirectory(out, NewLogger(io.Discard, false)); err == nil {
				t.Fatal("verify accepted a dangling latest.json")
			}
		})
	}
}

// --- FIX 7: the build log tells the truth ----------------------------------

// TestArchiveBuiltLogRecordsSizeAndToolchain asserts §11.2's package facts and
// §5.2's tool versions appear in the pack.archive.built event.
func TestArchiveBuiltLogRecordsSizeAndToolchain(t *testing.T) {
	configPath, launcher := fixture(t)
	var buf bytes.Buffer
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	pipeline, err := NewPipeline(cfg, NewLogger(&buf, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Run(Options{
		Platform: "linux-x64", Launcher: launcher, OutDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	kinds := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var doc map[string]any
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("log line is not JSON: %v", err)
		}
		if doc["event"] != EvArchiveBuilt {
			continue
		}
		kind, _ := doc["kind"].(string)
		kinds[kind] = true

		entries, _ := doc["entries"].(float64)
		files, _ := doc["files"].(float64)
		largest, _ := doc["largest"].(string)
		largestBytes, _ := doc["largest_bytes"].(float64)
		goVersion, _ := doc["go_version"].(string)
		if entries < 1 || files < 1 {
			t.Errorf("%s: entries=%v files=%v, want >= 1", kind, doc["entries"], doc["files"])
		}
		if largest == "" || largestBytes < 1 {
			t.Errorf("%s: largest=%q largest_bytes=%v", kind, doc["largest"], doc["largest_bytes"])
		}
		if goVersion == "" {
			t.Errorf("%s: go_version is missing from the build log (§5.2)", kind)
		}
		if _, ok := doc["zstd_version"]; !ok {
			t.Errorf("%s: zstd_version is missing from the build log (§5.2)", kind)
		}
	}
	if !kinds["zip"] || !kinds["tar.zst"] {
		t.Fatalf("pack.archive.built events missing: %v", kinds)
	}
}

// --- FIX 8: README.txt reports what the archive extracts --------------------

func TestReadmeExtractedSizeCountsTheWholeArchive(t *testing.T) {
	out := buildFixtureDist(t)
	zipPath := filepath.Join(out, "fixture-2026.09.1-linux-x64.zip")
	readme := zipEntry(t, zipPath, "Fixture/README.txt")

	match := regexp.MustCompile(`Extracted size:\s+([0-9]+) bytes`).FindSubmatch(readme)
	if match == nil {
		t.Fatal("README.txt has no Extracted size line")
	}
	claimed, err := strconv.ParseInt(string(match[1]), 10, 64)
	if err != nil {
		t.Fatal(err)
	}

	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var actual int64
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		actual += int64(f.UncompressedSize64)
	}
	if claimed != actual {
		t.Fatalf("README.txt claims %d extracted bytes; the ZIP extracts %d", claimed, actual)
	}

	// The manifest's total_size stays the payload the launcher checks against
	// the update archive: the sum of files[].size.
	var manifest ReleaseManifest
	if err := ReadJSON(filepath.Join(out, "fixture-2026.09.1.release.manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	var payload int64
	for _, f := range manifest.Files {
		payload += f.Size
	}
	if manifest.TotalSize != payload {
		t.Fatalf("manifest total_size=%d, files[] sum=%d", manifest.TotalSize, payload)
	}
}

// --- the reserved-name table mirrors the launcher's -------------------------

// TestReservedDeviceNamesArePinned pins deviceNames to the launcher's
// reservedNames set (launcher/internal/update/archive.go). The two modules
// cannot import each other, so the duplication is deliberate and this test is
// the tripwire: a change on either side must be made on both.
func TestReservedDeviceNamesArePinned(t *testing.T) {
	want := []string{"AUX", "CON", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6",
		"LPT7", "LPT8", "LPT9", "NUL", "PRN"}
	for i := 1; i <= 9; i++ {
		want = append(want, "COM"+strconv.Itoa(i))
	}
	sort.Strings(want)

	got := make([]string, 0, len(deviceNames))
	for name := range deviceNames {
		got = append(got, name)
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("deviceNames = %v, want %v (keep it in sync with the launcher's reservedNames)",
			got, want)
	}
}

// --- new helpers ------------------------------------------------------------

func buildFixtureDist(t *testing.T) string {
	t.Helper()
	pipeline, launcher := newFixturePipeline(t)
	out := t.TempDir()
	if _, err := pipeline.Run(Options{Platform: "linux-x64", Launcher: launcher, OutDir: out}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// regenerateSums rebuilds SHA256SUMS so a tampered dist gets past the checksum
// gate and reaches the manifest/index checks under test.
func regenerateSums(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "SHA256SUMS" {
			continue
		}
		names = append(names, entry.Name())
	}
	if err := WriteSha256Sums(dir, names, NewLogger(io.Discard, false)); err != nil {
		t.Fatal(err)
	}
}

func zipTopDir(t *testing.T, zipPath string) string {
	t.Helper()
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, f := range reader.File {
		parts := strings.SplitN(f.Name, "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			return parts[0]
		}
	}
	t.Fatalf("%s has no top-level directory", zipPath)
	return ""
}

// rewriteZipMember replaces one member's bytes and rewrites the archive.
func rewriteZipMember(t *testing.T, zipPath, target string, content []byte) {
	t.Helper()
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	found := false
	for _, f := range reader.File {
		hdr := f.FileHeader
		hdr.CRC32 = 0
		hdr.CompressedSize = 0
		hdr.UncompressedSize = 0
		hdr.CompressedSize64 = 0
		hdr.UncompressedSize64 = 0
		w, err := zw.CreateHeader(&hdr)
		if err != nil {
			t.Fatal(err)
		}
		if f.FileInfo().IsDir() {
			continue
		}
		raw := content
		if f.Name != target {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			raw, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
		} else {
			found = true
		}
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("%s is not in %s", target, zipPath)
	}
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
