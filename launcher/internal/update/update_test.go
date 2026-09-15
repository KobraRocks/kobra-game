package update

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/kobraerr"
)

// --- helpers ---------------------------------------------------------------

type tarEntry struct {
	Name     string
	Type     byte
	Body     []byte
	Linkname string
	Mode     int64
}

// buildArchive writes a .tar.zst archive into a fresh temp directory and
// returns its path.
func buildArchive(t *testing.T, entries ...tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if e.Type == 0 {
			e.Type = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.Name,
			Typeflag: e.Type,
			Linkname: e.Linkname,
			Mode:     e.Mode,
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if e.Type == tar.TypeReg {
			hdr.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.Name, err)
		}
		if e.Type == tar.TypeReg && len(e.Body) > 0 {
			if _, err := tw.Write(e.Body); err != nil {
				t.Fatalf("Write(%q): %v", e.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	path := filepath.Join(t.TempDir(), "payload.tar.zst")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	return path
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func goodManifest(total int64, files ...FileEntry) *Manifest {
	return &Manifest{
		Schema:        ReleaseManifestSchema,
		Release:       "2026.10.1",
		LauncherMin:   "1.0.0",
		EngineVersion: "1.0.0",
		SaveVersion:   1,
		TotalSize:     total,
		Files:         files,
	}
}

// goodManifestFor is goodManifest with the archive hash filled in from the
// archive the test actually built. §8.2.3.3 makes a manifest without one a hard
// failure, so a test that means to exercise a different rule has to get past
// that gate first — otherwise it passes for the wrong reason.
func goodManifestFor(t *testing.T, archive string, total int64, files ...FileEntry) *Manifest {
	t.Helper()
	m := goodManifest(total, files...)
	sum, _, err := hashFile(archive)
	if err != nil {
		t.Fatalf("hashFile(%s): %v", archive, err)
	}
	m.Archive = &ArchiveRef{Hash: sum}
	return m
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// assertNoPaths checks the §22.4 rule for one error: the caller-facing message
// and detail must not contain the temp directory the test used.
func assertNoPaths(t *testing.T, err error, secret string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var ke *kobraerr.KobraError
	if !errors.As(err, &ke) {
		t.Fatalf("error is %T, want *kobraerr.KobraError", err)
	}
	if strings.Contains(ke.Msg, secret) {
		t.Fatalf("error message leaks a path: %q", ke.Msg)
	}
	for k, v := range ke.Detail {
		if s, ok := v.(string); ok && strings.Contains(s, secret) {
			t.Fatalf("error detail %q leaks a path: %q", k, s)
		}
	}
	if ke.Cause == nil {
		t.Errorf("expected the path to be preserved in Cause")
	}
}

// --- Extract ---------------------------------------------------------------

func TestExtractRejectsUnsafeEntries(t *testing.T) {
	cases := []struct {
		name string
		ent  tarEntry
	}{
		{"dotdot escape", tarEntry{Name: "../escape", Body: []byte("x")}},
		{"nested dotdot escape", tarEntry{Name: "game/../../escape", Body: []byte("x")}},
		{"absolute path", tarEntry{Name: "/etc/passwd", Body: []byte("x")}},
		{"symlink", tarEntry{Name: "game/link", Type: tar.TypeSymlink, Linkname: "/etc/passwd"}},
		{"hardlink", tarEntry{Name: "game/hard", Type: tar.TypeLink, Linkname: "game/other"}},
		{"char device", tarEntry{Name: "game/dev", Type: tar.TypeChar}},
		{"device name CON", tarEntry{Name: "game/CON", Body: []byte("x")}},
		{"device name con.txt", tarEntry{Name: "game/con.txt", Body: []byte("x")}},
		{"device name in nested", tarEntry{Name: "game/logs/LPT1", Body: []byte("x")}},
		{"data entry", tarEntry{Name: "data/saves/slot1.json", Body: []byte("{}")}},
		{"data entry uppercase", tarEntry{Name: "DATA/saves/slot1.json", Body: []byte("{}")}},
		{"data via traversal", tarEntry{Name: "game/../data/saves/s.json", Body: []byte("{}")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := buildArchive(t, tc.ent)
			dest := filepath.Join(t.TempDir(), "game.new")
			m := goodManifest(0)
			// Size budget is irrelevant here; the entry must be rejected on
			// its name or type.
			m.TotalSize = 1 << 20
			err := Extract(archive, dest, m)
			if err == nil {
				t.Fatalf("Extract accepted %s", tc.name)
			}
			assertNoPaths(t, err, filepath.Dir(dest))
			if tc.ent.Name == "data/saves/slot1.json" || strings.HasPrefix(tc.ent.Name, "data/") || strings.HasPrefix(tc.ent.Name, "DATA/") || tc.ent.Name == "game/../data/saves/s.json" {
				var ke *kobraerr.KobraError
				if errors.As(err, &ke) && ke.Detail["reason"] != "data_dir" {
					t.Fatalf("data entry rejected with reason %v, want data_dir", ke.Detail["reason"])
				}
			}
			if exists(filepath.Join(dest, "data")) {
				t.Fatalf("Extract created a data directory")
			}
		})
	}
}

func TestExtractRejectsOversizedExpansion(t *testing.T) {
	body := bytes.Repeat([]byte("A"), 4096)
	archive := buildArchive(t, tarEntry{Name: "game/blob.bin", Body: body})
	dest := filepath.Join(t.TempDir(), "game.new")
	m := goodManifest(1024) // declared budget smaller than the payload

	err := Extract(archive, dest, m)
	if err == nil {
		t.Fatal("Extract accepted an archive larger than the declared budget")
	}
	assertNoPaths(t, err, filepath.Dir(dest))

	var ke *kobraerr.KobraError
	if !errors.As(err, &ke) || ke.Detail["reason"] != "size_budget" {
		t.Fatalf("reason = %v, want size_budget", ke.Detail["reason"])
	}
	if exists(filepath.Join(dest, "game", "blob.bin")) {
		t.Fatal("Extract wrote a file that exceeded the budget")
	}
}

func TestExtractSucceedsOnGoodArchive(t *testing.T) {
	index := []byte("<!doctype html><title>kobra</title>")
	script := []byte("console.log('hi')")
	runner := []byte("#!/bin/sh\nexec ./game\n")
	archive := buildArchive(t,
		tarEntry{Name: "game/index.html", Body: index},
		tarEntry{Name: "game/shell.js", Body: script},
		tarEntry{Name: "launcher/run.sh", Body: runner, Mode: 0o755},
	)
	m := goodManifest(int64(len(index)+len(script)+len(runner)),
		FileEntry{Path: "game/index.html", Size: int64(len(index)), Hash: sha256Of(index)},
		FileEntry{Path: "game/shell.js", Size: int64(len(script)), Hash: sha256Of(script)},
		FileEntry{Path: "launcher/run.sh", Size: int64(len(runner)), Hash: sha256Of(runner), Mode: "755"},
	)

	// Updater spec §9.2 step 10: the staging root receives game.new/ and
	// launcher.new/, with the archive's prefixes stripped, because the swap
	// renames game.new/ over game/ and merges launcher.new/ into launcher/.
	stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
	if err := Extract(archive, stagingRoot, m); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	newGame := filepath.Join(stagingRoot, stagingDirName)
	if got := readFile(t, filepath.Join(newGame, "index.html")); got != string(index) {
		t.Fatalf("game.new/index.html = %q", got)
	}
	if got := readFile(t, filepath.Join(newGame, "shell.js")); got != string(script) {
		t.Fatalf("game.new/shell.js = %q", got)
	}
	// The "game/" prefix must NOT survive into the staged tree: a nested
	// game.new/game/ is installed as game/game/ and the release cannot start.
	if exists(filepath.Join(newGame, gameDirName)) {
		t.Fatal("the archive's game/ prefix survived extraction; the swap would nest it")
	}
	newLauncher := filepath.Join(stagingRoot, launcherStagingDirName)
	if got := readFile(t, filepath.Join(newLauncher, "run.sh")); got != string(runner) {
		t.Fatalf("launcher.new/run.sh = %q", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(newLauncher, "run.sh"))
		if err != nil {
			t.Fatalf("stat run.sh: %v", err)
		}
		if st.Mode().Perm() != 0o755 {
			t.Fatalf("run.sh mode = %v, want 0755", st.Mode().Perm())
		}
	}
}

// TestExtractRoutesMembersToStaging pins the routing table itself, including
// the prefixes that must be refused.
func TestExtractRoutesMembersToStaging(t *testing.T) {
	root := filepath.Join(t.TempDir(), UpdateDirName)
	cases := []struct {
		rel     string
		want    string
		wantErr bool
	}{
		{"game/index.html", filepath.Join(root, stagingDirName, "index.html"), false},
		{"game/a/b/c.js", filepath.Join(root, stagingDirName, "a", "b", "c.js"), false},
		{"launcher/launcher", filepath.Join(root, launcherStagingDirName, "launcher"), false},
		{"data/saves/s1.json", "", true},
		{"README.txt", "", true},
		{"game", "", true},
		{"game/", "", true},
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := stagedTarget(root, tc.rel)
		if (err != nil) != tc.wantErr {
			t.Fatalf("stagedTarget(%q) err = %v, wantErr = %v", tc.rel, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Fatalf("stagedTarget(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

// TestMergeLauncherIntoReplacesInPlace covers Packaging spec §9.3: the launcher
// tree is replaced file by file, keeping unrelated files and the executable bit.
func TestMergeLauncherIntoReplacesInPlace(t *testing.T) {
	folder := t.TempDir()
	stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
	writeFile(t, filepath.Join(folder, "launcher", "launcher"), "old-binary")
	writeFile(t, filepath.Join(folder, "launcher", "keep.txt"), "keep me")
	writeFile(t, filepath.Join(stagingRoot, launcherStagingDirName, "launcher"), "new-binary")
	writeFile(t, filepath.Join(stagingRoot, launcherStagingDirName, "launcher.config.json"), `{"release":"2026.10.1"}`)
	if err := os.Chmod(filepath.Join(stagingRoot, launcherStagingDirName, "launcher"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := MergeLauncherInto(stagingRoot, folder); err != nil {
		t.Fatalf("MergeLauncherInto: %v", err)
	}
	if got := readFile(t, filepath.Join(folder, "launcher", "launcher")); got != "new-binary" {
		t.Fatalf("launcher = %q, want the staged binary", got)
	}
	if got := readFile(t, filepath.Join(folder, "launcher", "launcher.config.json")); !strings.Contains(got, "2026.10.1") {
		t.Fatalf("launcher.config.json = %q, want the staged config", got)
	}
	// A file the release does not ship is not removed: launcher/ is merged, not
	// swapped away.
	if got := readFile(t, filepath.Join(folder, "launcher", "keep.txt")); got != "keep me" {
		t.Fatalf("keep.txt = %q, want it preserved", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(folder, "launcher", "launcher"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if st.Mode().Perm()&0o100 == 0 {
			t.Fatalf("launcher mode %v lost the executable bit", st.Mode().Perm())
		}
	}
}

// TestMergeLauncherIntoAbsentIsFine pins R14.6's neighbour: a release with no
// launcher change ships no launcher tree, which is normal rather than an error.
func TestMergeLauncherIntoAbsentIsFine(t *testing.T) {
	folder := t.TempDir()
	stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
	if err := MergeLauncherInto(stagingRoot, folder); err != nil {
		t.Fatalf("MergeLauncherInto with no staged launcher: %v", err)
	}
}

func TestExtractRejectsDataEntryAndLogsEvent(t *testing.T) {
	logDir := t.TempDir()
	logger, _ := diagnostics.Open(diagnostics.Options{LogDir: logDir, Level: diagnostics.LevelDebug})
	SetLogger(logger)
	defer SetLogger(nil)

	archive := buildArchive(t, tarEntry{Name: "game/index.html", Body: []byte("ok")}, tarEntry{Name: "data/saves/s1.json", Body: []byte("{}")})
	dest := filepath.Join(t.TempDir(), "game.new")
	m := goodManifest(1 << 20)

	err := Extract(archive, dest, m)
	if err == nil {
		t.Fatal("Extract accepted an archive that touches data/")
	}
	var ke *kobraerr.KobraError
	if !errors.As(err, &ke) || ke.Detail["reason"] != "data_dir" {
		t.Fatalf("reason = %v, want data_dir", ke.Detail["reason"])
	}
	if ke.Msg != msgData {
		t.Fatalf("message = %q, want the E30 wording", ke.Msg)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("logger close: %v", err)
	}
	logged := readFile(t, filepath.Join(logDir, "launcher.log"))
	if !strings.Contains(logged, "update.data.rejected") {
		t.Fatalf("log does not contain update.data.rejected:\n%s", logged)
	}
}

// --- Verify ----------------------------------------------------------------

func TestVerifyCatchesHashMismatch(t *testing.T) {
	body := []byte("hello world")
	archive := buildArchive(t, tarEntry{Name: "game/a.txt", Body: body})
	archiveSum, _, err := hashFile(archive)
	if err != nil {
		t.Fatalf("hashFile: %v", err)
	}

	t.Run("wrong declared hash", func(t *testing.T) {
		wrong := "sha256:" + strings.Repeat("0", 64)
		if wrong == archiveSum {
			t.Fatal("test bug: sentinel equals the real hash")
		}
		m := goodManifest(int64(len(body)))
		m.Archive = &ArchiveRef{Hash: wrong, Size: 0}
		err := Verify(archive, m)
		if err == nil {
			t.Fatal("Verify accepted a hash mismatch")
		}
		var ke *kobraerr.KobraError
		if !errors.As(err, &ke) || ke.Detail["reason"] != "archive_hash" {
			t.Fatalf("reason = %v, want archive_hash", ke.Detail["reason"])
		}
		assertNoPaths(t, err, filepath.Dir(archive))
	})

	t.Run("matching hash", func(t *testing.T) {
		m := goodManifest(int64(len(body)))
		m.Archive = &ArchiveRef{Hash: archiveSum}
		if err := Verify(archive, m); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})

	t.Run("per-file index mismatch", func(t *testing.T) {
		m := goodManifestFor(t, archive, int64(len(body)),
			FileEntry{Path: "game/a.txt", Size: int64(len(body)), Hash: "sha256:" + strings.Repeat("1", 64)})
		err := Verify(archive, m)
		if err == nil {
			t.Fatal("Verify accepted a per-file hash mismatch")
		}
		var ke *kobraerr.KobraError
		if !errors.As(err, &ke) || ke.Detail["reason"] != "entry_content" {
			t.Fatalf("reason = %v, want entry_content", ke.Detail["reason"])
		}
	})

	t.Run("unlisted archive member", func(t *testing.T) {
		m := goodManifestFor(t, archive, int64(len(body)),
			FileEntry{Path: "game/other.txt", Size: int64(len(body)), Hash: sha256Of(body)})
		err := Verify(archive, m)
		if err == nil {
			t.Fatal("Verify accepted an unlisted archive member")
		}
		var ke *kobraerr.KobraError
		if !errors.As(err, &ke) || ke.Detail["reason"] != "unlisted_entry" {
			t.Fatalf("reason = %v, want unlisted_entry", ke.Detail["reason"])
		}
	})
}

// TestVerifyRejectsMissingArchiveHash covers §8.2.3.3: a manifest that does not
// describe the archive it accompanies must be refused outright, not silently
// downgraded to per-file verification.
func TestVerifyRejectsMissingArchiveHash(t *testing.T) {
	body := []byte("hello world")
	archive := buildArchive(t, tarEntry{Name: "game/a.txt", Body: body})

	// The index is complete and correct, so per-file verification alone would
	// have succeeded. The missing archive hash is the only defect.
	m := goodManifest(int64(len(body)),
		FileEntry{Path: "game/a.txt", Size: int64(len(body)), Hash: sha256Of(body)})
	err := Verify(archive, m)
	if err == nil {
		t.Fatal("Verify accepted a manifest with no archive hash")
	}
	var ke *kobraerr.KobraError
	if !errors.As(err, &ke) || ke.Detail["reason"] != "archive_hash_missing" {
		t.Fatalf("reason = %v, want archive_hash_missing", ke.Detail["reason"])
	}
	if ke.Msg != msgIncomplete {
		t.Fatalf("message = %q, want the incomplete-manifest wording", ke.Msg)
	}
	// A release defect must not be reported as a damaged download.
	if strings.Contains(ke.Msg, "Re-download") {
		t.Fatalf("message = %q tells the user to re-download a release defect", ke.Msg)
	}
	assertNoPaths(t, err, filepath.Dir(archive))

	// The flat archive_sha256 alias is still honoured (§8.2.2).
	m.ArchiveSHA256 = sha256Of(body)
	// The alias names the wrong bytes, so this now fails on the hash, not on
	// absence — which is what proves the alias was read.
	err = Verify(archive, m)
	if err == nil {
		t.Fatal("Verify accepted a mismatching archive_sha256 alias")
	}
	if !errors.As(err, &ke) || ke.Detail["reason"] != "archive_hash" {
		t.Fatalf("reason = %v, want archive_hash", ke.Detail["reason"])
	}
}

// TestVerifyRejectsEmbeddedReleaseManifest covers §6.4: the release manifest
// cannot be inside the archive it indexes, because the index cannot contain the
// manifest's own hash and an unlisted member is rejected. The launcher says so
// explicitly instead of reporting a generic unsafe entry.
func TestVerifyRejectsEmbeddedReleaseManifest(t *testing.T) {
	body := []byte("index")
	archive := buildArchive(t,
		tarEntry{Name: ReleaseManifestName, Body: []byte(`{"schema":"kobra.release-manifest/1"}`)},
		tarEntry{Name: "game/a.txt", Body: body},
	)
	// The index lists every member except the manifest, which is exactly what
	// §6.4 tells a publisher to produce — and exactly what cannot work.
	m := goodManifestFor(t, archive, int64(len(body)),
		FileEntry{Path: "game/a.txt", Size: int64(len(body)), Hash: sha256Of(body)})

	err := Verify(archive, m)
	if err == nil {
		t.Fatal("Verify accepted an archive containing the release manifest")
	}
	var ke *kobraerr.KobraError
	if !errors.As(err, &ke) || ke.Detail["reason"] != "release_manifest_in_archive" {
		t.Fatalf("reason = %v, want release_manifest_in_archive", ke.Detail["reason"])
	}
	if ke.Msg != msgEmbeddedManifest {
		t.Fatalf("message = %q, want the embedded-manifest wording", ke.Msg)
	}

	// Extract enforces the same rule, so a caller that skipped Verify is still
	// safe.
	dest := filepath.Join(t.TempDir(), "game.new")
	err = Extract(archive, dest, m)
	if err == nil {
		t.Fatal("Extract accepted an archive containing the release manifest")
	}
	if !errors.As(err, &ke) || ke.Detail["reason"] != "release_manifest_in_archive" {
		t.Fatalf("Extract reason = %v, want release_manifest_in_archive", ke.Detail["reason"])
	}
	if exists(filepath.Join(dest, filepath.FromSlash(ReleaseManifestName))) {
		t.Fatal("Extract wrote the embedded release manifest")
	}
}

func TestVerifyCatchesSizeMismatch(t *testing.T) {
	body := []byte("12345")
	archive := buildArchive(t, tarEntry{Name: "game/a.txt", Body: body})

	t.Run("declared total too large", func(t *testing.T) {
		m := goodManifestFor(t, archive, int64(len(body))+1)
		err := Verify(archive, m)
		if err == nil {
			t.Fatal("Verify accepted a total_size mismatch")
		}
		var ke *kobraerr.KobraError
		if !errors.As(err, &ke) || ke.Detail["reason"] != "total_size" {
			t.Fatalf("reason = %v, want total_size", ke.Detail["reason"])
		}
		assertNoPaths(t, err, filepath.Dir(archive))
	})

	t.Run("declared compressed size mismatch", func(t *testing.T) {
		sum, _, err := hashFile(archive)
		if err != nil {
			t.Fatalf("hashFile: %v", err)
		}
		m := goodManifest(int64(len(body)))
		m.Archive = &ArchiveRef{Hash: sum, Size: 1}
		err = Verify(archive, m)
		if err == nil {
			t.Fatal("Verify accepted a compressed-size mismatch")
		}
		var ke *kobraerr.KobraError
		if !errors.As(err, &ke) || ke.Detail["reason"] != "archive_size" {
			t.Fatalf("reason = %v, want archive_size", ke.Detail["reason"])
		}
	})

	t.Run("matching size", func(t *testing.T) {
		m := goodManifestFor(t, archive, int64(len(body)))
		if err := Verify(archive, m); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
}

func TestVerifyFailsClosedOnDeclaredSignature(t *testing.T) {
	archive := buildArchive(t)
	m := goodManifestFor(t, archive, 0)
	m.Signatures = &Signatures{GPG: "-----BEGIN PGP SIGNATURE-----\nabc\n-----END PGP SIGNATURE-----"}

	err := Verify(archive, m)
	if err == nil {
		t.Fatal("Verify accepted a signed manifest without a verifier")
	}
	var ke *kobraerr.KobraError
	if !errors.As(err, &ke) || ke.Detail["reason"] != "signature_unavailable" {
		t.Fatalf("reason = %v, want signature_unavailable", ke.Detail["reason"])
	}
	if !strings.Contains(ke.Msg, "signature could not be verified") {
		t.Fatalf("message = %q, want the signature wording", ke.Msg)
	}

	// With a verifier installed the manifest is accepted.
	SetSignatureVerifier(func([]byte, *Manifest) error { return nil })
	defer SetSignatureVerifier(nil)
	if err := Verify(archive, m); err != nil {
		t.Fatalf("Verify with verifier: %v", err)
	}

	// A verifier that rejects the manifest is honoured.
	SetSignatureVerifier(func([]byte, *Manifest) error { return errors.New("bad signature") })
	if err := Verify(archive, m); err == nil {
		t.Fatal("Verify accepted a manifest a verifier rejected")
	}
}

func TestVerifyRejectsDataEntry(t *testing.T) {
	archive := buildArchive(t, tarEntry{Name: "data/x", Body: []byte("x")})
	m := goodManifestFor(t, archive, 1)
	err := Verify(archive, m)
	if err == nil {
		t.Fatal("Verify accepted an archive containing data/")
	}
	var ke *kobraerr.KobraError
	if !errors.As(err, &ke) || ke.Detail["reason"] != "data_dir" {
		t.Fatalf("reason = %v, want data_dir", ke.Detail["reason"])
	}
}

// --- Swap and Recover ------------------------------------------------------

func TestSwapHappyPath(t *testing.T) {
	folder := t.TempDir()
	// Updater spec §5 / R5.3: the staged release lives in the sidecar update
	// directory, not in the game folder, so a copied folder carries no staging.
	stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
	writeFile(t, filepath.Join(folder, "game", "index.html"), "old")
	writeFile(t, filepath.Join(stagingRoot, "game.new", "index.html"), "new")
	writeFile(t, filepath.Join(folder, "data", "saves", "s1.json"), `{"slot":1}`)

	if err := Swap(folder, stagingRoot, "2026.10.1"); err != nil {
		t.Fatalf("Swap: %v", err)
	}

	if got := readFile(t, filepath.Join(folder, "game", "index.html")); got != "new" {
		t.Fatalf("game/index.html = %q, want the staged release", got)
	}
	if got := readFile(t, filepath.Join(folder, "game.old", "index.html")); got != "old" {
		t.Fatalf("game.old/index.html = %q, want the previous release", got)
	}
	if exists(filepath.Join(stagingRoot, "game.new")) {
		t.Fatal("game.new still exists after a successful swap")
	}
	if exists(filepath.Join(folder, stateFileName)) {
		t.Fatal("update.state still exists after a successful swap")
	}
	if got := readFile(t, filepath.Join(folder, "data", "saves", "s1.json")); got != `{"slot":1}` {
		t.Fatalf("data/ was modified: %q", got)
	}
}

func TestSwapRequiresStaging(t *testing.T) {
	folder := t.TempDir()
	stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
	writeFile(t, filepath.Join(folder, "game", "index.html"), "old")
	err := Swap(folder, stagingRoot, "2026.10.1")
	if err == nil {
		t.Fatal("Swap succeeded without game.new")
	}
	assertNoPaths(t, err, folder)
	if exists(filepath.Join(folder, stateFileName)) {
		t.Fatal("Swap wrote update.state before checking the staging directory")
	}
}

func TestRecoverMatrix(t *testing.T) {
	const stateJSON = `{"schema":"kobra.update-state/1","release":"2026.10.1","step":"begin",` +
		`"from":"game","to":"game.old","staging":"game.new","started":"2026-10-01T12:00:00Z"}`

	tests := []struct {
		name       string
		game       string // content marker, "" means the directory is absent
		newRelease string
		oldRelease string
		state      bool
		want       RecoveryAction
		wantGame   string
		wantOld    string // "" means the directory must be gone, "-" means anything
		wantNew    bool
	}{
		{
			name:       "bullet 1: staging completes the swap",
			newRelease: "new",
			want:       RecoveryRolledForward,
			wantGame:   "new",
			wantOld:    "",
		},
		{
			name:       "bullet 1: staging completes with a backup already present",
			newRelease: "new",
			oldRelease: "old",
			want:       RecoveryRolledForward,
			wantGame:   "new",
			wantOld:    "old",
		},
		{
			name:       "bullet 2: backup rolls back",
			oldRelease: "old",
			want:       RecoveryRolledBack,
			wantGame:   "old",
			wantOld:    "",
		},
		{
			name:       "bullet 3: game+old+state with staging intact rolls forward",
			game:       "cur",
			newRelease: "new",
			oldRelease: "stale",
			state:      true,
			want:       RecoveryRolledForward,
			wantGame:   "new",
			wantOld:    "cur",
		},
		{
			name:       "bullet 3: game+old+state without staging rolls back",
			game:       "new",
			oldRelease: "old",
			state:      true,
			want:       RecoveryRolledBack,
			wantGame:   "old",
			wantOld:    "",
		},
		{
			name:       "bullet 4: stray staging is cleaned",
			game:       "cur",
			newRelease: "new",
			want:       RecoveryCleanedStray,
			wantGame:   "cur",
			wantNew:    false,
		},
		{
			name:       "bullet 4: a retained backup beside game is preserved",
			game:       "cur",
			oldRelease: "old",
			want:       RecoveryNone,
			wantGame:   "cur",
			wantOld:    "old",
		},
		{
			name: "bullet 4: nothing to do",
			want: RecoveryNone,
		},
		{
			name:  "state without any move aborts cleanly",
			game:  "cur",
			state: true,
			want:  RecoveryNone,
			// game.new was never staged, so there is nothing to clean.
			wantNew:  false,
			wantGame: "cur",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			folder := t.TempDir()
			stagingRoot := filepath.Join(t.TempDir(), UpdateDirName)
			if tc.game != "" {
				writeFile(t, filepath.Join(folder, gameDirName, "index.html"), tc.game)
			}
			if tc.newRelease != "" {
				writeFile(t, filepath.Join(stagingRoot, stagingDirName, "index.html"), tc.newRelease)
			}
			if tc.oldRelease != "" {
				writeFile(t, filepath.Join(folder, backupDirName, "index.html"), tc.oldRelease)
			}
			if tc.state {
				writeFile(t, filepath.Join(folder, stateFileName), stateJSON)
			}

			got, err := Recover(folder, stagingRoot)
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Recover = %q, want %q", got, tc.want)
			}
			if tc.wantGame != "" {
				if got := readFile(t, filepath.Join(folder, gameDirName, "index.html")); got != tc.wantGame {
					t.Fatalf("game content = %q, want %q", got, tc.wantGame)
				}
			}
			switch tc.wantOld {
			case "":
				if exists(filepath.Join(folder, backupDirName)) {
					t.Fatal("game.old still exists")
				}
			case "-":
			default:
				if got := readFile(t, filepath.Join(folder, backupDirName, "index.html")); got != tc.wantOld {
					t.Fatalf("game.old content = %q, want %q", got, tc.wantOld)
				}
			}
			if !tc.wantNew && exists(filepath.Join(stagingRoot, stagingDirName)) {
				t.Fatal("game.new still exists")
			}
			// Every branch of Recover removes the marker once it has acted:
			// the only case that keeps it is the unrecoverable one, covered by
			// TestRecoverUnrecoverable.
			if exists(filepath.Join(folder, stateFileName)) {
				t.Fatal("update.state still exists")
			}
		})
	}
}

func TestRecoverRemovesStateButLeavesBackup(t *testing.T) {
	folder := t.TempDir()
	writeFile(t, filepath.Join(folder, gameDirName, "index.html"), "cur")
	writeFile(t, filepath.Join(folder, backupDirName, "index.html"), "old")

	action, err := Recover(folder, filepath.Join(t.TempDir(), UpdateDirName))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// FR-UPD-8: the rollback copy survives a clean restart. This is the
	// documented reading of §19.4 bullet 4's "stray game.old".
	if action != RecoveryNone {
		t.Fatalf("Recover = %q, want %q", action, RecoveryNone)
	}
	if !exists(filepath.Join(folder, backupDirName)) {
		t.Fatal("game.old was deleted; rollback would be impossible")
	}
}

func TestRecoverUnrecoverable(t *testing.T) {
	folder := t.TempDir()
	writeFile(t, filepath.Join(folder, stateFileName), `{"schema":"kobra.update-state/1","release":"2026.10.1"}`)

	_, err := Recover(folder, filepath.Join(t.TempDir(), UpdateDirName))
	if err == nil {
		t.Fatal("Recover repaired a folder with no game, no staging and no backup")
	}
	if !exists(filepath.Join(folder, stateFileName)) {
		t.Fatal("Recover removed the marker of an unrecoverable swap")
	}
}

// --- manifest fetching and Check ------------------------------------------

func TestFetchLatestFetchManifestAndCheck(t *testing.T) {
	manifestDoc := map[string]any{
		"schema":         ReleaseManifestSchema,
		"release":        "2026.10.1",
		"launcher_min":   "1.0.0",
		"engine_version": "1.0.0",
		"save_version":   1,
		"total_size":     3,
		"files": []map[string]any{
			{"path": "game/a.txt", "size": 3, "hash": sha256Of([]byte("abc"))},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"release":      "2026.10.1",
			"manifest_url": "/release.manifest.json",
			"notes_url":    "https://example.invalid/notes",
		})
	})
	mux.HandleFunc("/release.manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(manifestDoc)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	latest, err := FetchLatest(ctx, srv.URL)
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if latest.Release != "2026.10.1" {
		t.Fatalf("release = %q", latest.Release)
	}
	if latest.ManifestURL != srv.URL+"/release.manifest.json" {
		t.Fatalf("manifest_url = %q, want it resolved against the base", latest.ManifestURL)
	}

	m, err := FetchManifest(ctx, latest.ManifestURL)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if m.Release != "2026.10.1" || len(m.Files) != 1 || m.TotalSize != 3 {
		t.Fatalf("manifest = %+v", m)
	}
	if m.raw == nil {
		t.Fatal("FetchManifest did not retain the raw document for signature checking")
	}

	res, err := Check(ctx, srv.URL, "2026.09.1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Available || res.Latest != "2026.10.1" || res.Current != "2026.09.1" {
		t.Fatalf("Check = %+v", res)
	}
	if res.NotesURL != "https://example.invalid/notes" {
		t.Fatalf("notes_url = %q", res.NotesURL)
	}

	res, err = Check(ctx, srv.URL, "2026.10.1")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Available {
		t.Fatal("Check reported an update although the installed release is current")
	}
}

func TestFetchLatestRejectsBadBase(t *testing.T) {
	if _, err := FetchLatest(context.Background(), ""); err == nil {
		t.Fatal("FetchLatest accepted an empty base URL")
	}
}

func TestFetchLatestRejectsOversizedDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), int(maxDocumentBytes)+1))
	}))
	defer srv.Close()

	if _, err := FetchLatest(context.Background(), srv.URL); err == nil {
		t.Fatal("FetchLatest accepted a response larger than the document limit")
	}
}

func TestMeetsLauncherMin(t *testing.T) {
	m := goodManifest(0)
	m.LauncherMin = "1.4.0"

	tests := []struct {
		version string
		want    bool
	}{
		{"1.4.0", true},
		{"1.4.1", true},
		{"1.5", true},
		{"2.0.0", true},
		{"1.3.9", false},
		{"not-a-version", false},
		// VERSIONING.md: version fields are bare X.Y.Z and an unparseable value
		// fails closed. 2.0.0-rc.1 is refused because prerelease suffixes are not
		// supported — `channel` carries pre-release — not because it sorts low.
		{"0.1.0-dev", false},
		{"2.0.0-rc.1", false},
		{"1.4.0+build", false},
	}
	for _, tc := range tests {
		ok, msg := MeetsLauncherMin(m, tc.version)
		if ok != tc.want {
			t.Fatalf("MeetsLauncherMin(%q) = %v, want %v", tc.version, ok, tc.want)
		}
		if !ok && msg != LauncherTooOldMessage {
			t.Fatalf("refusal message = %q, want the E29 wording", msg)
		}
		if ok && msg != "" {
			t.Fatalf("refusal message = %q, want empty", msg)
		}
	}

	if ok, _ := MeetsLauncherMin(nil, "0.0.1"); !ok {
		t.Fatal("a nil manifest should not gate the update")
	}
	if err := GuardApply(m, "1.0.0"); err == nil {
		t.Fatal("GuardApply allowed a too-old launcher")
	} else {
		var ke *kobraerr.KobraError
		if !errors.As(err, &ke) || ke.Msg != LauncherTooOldMessage {
			t.Fatalf("GuardApply error = %v, want the E29 wording", err)
		}
	}
	if err := GuardApply(m, "1.4.0"); err != nil {
		t.Fatalf("GuardApply refused a new-enough launcher: %v", err)
	}
}

// TestMalformedLauncherMinCannotDisableTheFloor pins the fail-closed rule for the
// floor itself, not only for the launcher's own version.
//
// compareVersions orders an unparseable version as older than every numeric one.
// As an upper bound that is the safe direction, but launcher_min IS a floor: an
// unreadable floor reads as older than any launcher and satisfies the gate
// unconditionally, so one malformed character in a manifest would remove FR-UPD-6
// altogether. FetchManifest refuses such a manifest first; this keeps the gate
// safe for callers that construct a Manifest directly.
func TestMalformedLauncherMinCannotDisableTheFloor(t *testing.T) {
	for _, bad := range []string{"garbage", "1.0.0-rc.1", "1.x.0", "v", "."} {
		m := goodManifest(0)
		m.LauncherMin = bad
		ok, msg := MeetsLauncherMin(m, "99.0.0")
		if ok {
			t.Fatalf("launcher_min %q disabled the floor for launcher 99.0.0", bad)
		}
		if msg != LauncherTooOldMessage {
			t.Fatalf("launcher_min %q refusal message = %q, want the E29 wording", bad, msg)
		}
	}

	// The documented leniency has to survive: an absent floor is not a requirement.
	m := goodManifest(0)
	m.LauncherMin = ""
	if ok, _ := MeetsLauncherMin(m, "0.0.1"); !ok {
		t.Fatal("an empty launcher_min should not gate the update")
	}
}

// TestFetchManifestRejectsUnreadableLauncherMin closes the same hole one layer
// up: a manifest whose floor cannot be read is refused as malformed, alongside
// the existing checks on `release` and `schema`.
func TestFetchManifestRejectsUnreadableLauncherMin(t *testing.T) {
	manifestDoc := map[string]any{
		"schema":         ReleaseManifestSchema,
		"release":        "2026.10.1",
		"launcher_min":   "1.0.0-rc.1",
		"engine_version": "1.0.0",
		"save_version":   1,
		"total_size":     0,
		"files":          []map[string]any{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(manifestDoc)
	}))
	defer srv.Close()

	ctx := context.Background()
	if _, err := FetchManifest(ctx, srv.URL); err == nil {
		t.Fatal("FetchManifest accepted a manifest whose launcher_min cannot be read")
	}

	// A well-formed floor still passes, so the check is not simply refusing.
	manifestDoc["launcher_min"] = "1.0.0"
	if _, err := FetchManifest(ctx, srv.URL); err != nil {
		t.Fatalf("FetchManifest refused a well-formed launcher_min: %v", err)
	}

	// And the documented leniency survives: an absent floor is not a requirement.
	delete(manifestDoc, "launcher_min")
	if _, err := FetchManifest(ctx, srv.URL); err != nil {
		t.Fatalf("FetchManifest refused a manifest with no launcher_min: %v", err)
	}
}

func TestReleaseNewer(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"2026.10.1", "2026.09.1", true},
		{"2026.09.1", "2026.10.1", false},
		{"2026.09.1", "2026.09.1", false},
		{"2026.10.2", "2026.10.1", true},
		{"2026.10.10", "2026.10.9", true},
		{"", "2026.10.1", false},
		{"garbage", "2026.10.1", false},
		{"2026.10.1", "garbage", true},
	}
	for _, tc := range tests {
		if got := ReleaseNewer(tc.a, tc.b); got != tc.want {
			t.Fatalf("ReleaseNewer(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
