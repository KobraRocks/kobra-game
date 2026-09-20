package storage

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// tarEntry is one entry of a synthetic mod archive.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

// tarZstd builds the .tar.zst a modder's tooling produces, in the given order.
//
// `extractTarZstd` had no test before this file. It is the path that unpacks
// caller-supplied bytes onto the player's disk, so its contract — the layout it
// accepts and the entries it refuses — is asserted here rather than assumed from
// the reader's code.
func tarZstd(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	tw := tar.NewWriter(zw)
	for _, entry := range entries {
		flag := entry.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     entry.name,
			Mode:     0o644,
			Typeflag: flag,
			Linkname: entry.linkname,
		}
		if flag == tar.TypeReg {
			hdr.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", entry.name, err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatalf("tar body %q: %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	return buf.Bytes()
}

// TestModArchiveLayoutIsTheModTreeAtTheArchiveRoot pins the layout §27.5 left
// open: the archive holds the mod's own file tree — `mod.manifest.json` and
// `assets/…` — with no wrapper directory, because `installModLocked` extracts
// into `data/mods/<id>/` and that is where the game reads `mod.manifest.json`
// (`03:03.6`).
func TestModArchiveLayoutIsTheModTreeAtTheArchiveRoot(t *testing.T) {
	e, _ := newEngine(t)
	archive := tarZstd(t, []tarEntry{
		{name: "mod.manifest.json", body: `{"schema":"kobra.mod-manifest/1","id":"noir-overhaul"}`},
		{name: "assets/data/mod.json", body: `{"schema":"worldspiracy.mod-index/1","id":"noir-overhaul"}`},
		{name: "assets/data/packs/noir-gear.pack.json", body: `{}`},
		{name: "assets/textures/ui/frame.png", body: "png"},
	})
	if err := e.ModAction(ModAction{
		ID:           "noir-overhaul",
		Action:       "install",
		ArchiveBytes: archive,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}

	root := filepath.Join(e.dirFor("mods"), "noir-overhaul")
	for _, rel := range []string{
		"mod.manifest.json",
		"assets/data/mod.json",
		"assets/data/packs/noir-gear.pack.json",
		"assets/textures/ui/frame.png",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("the mod tree must land %s at the mod root: %v", rel, err)
		}
	}
	// An install enables the mod, which is what makes it resolvable.
	enabled := e.EnabledModIDs()
	if len(enabled) != 1 || enabled[0] != "noir-overhaul" {
		t.Errorf("an installed mod is enabled, got %v", enabled)
	}
}

// TestModArchiveRejectsEscapesLinksAndDeviceNames covers the refusal branches
// `extractTarZstd` documents (§19.3, FR-AST-12, FS threat T14): an archive may
// not write outside its own directory, may not carry anything but regular files
// and directories, and may not smuggle a reserved device name.
func TestModArchiveRejectsEscapesLinksAndDeviceNames(t *testing.T) {
	cases := []struct {
		name  string
		entry tarEntry
	}{
		{"a path that traverses out", tarEntry{name: "../escape.txt", body: "x"}},
		{"a deeper traversal", tarEntry{name: "assets/../../escape.txt", body: "x"}},
		{"an absolute path", tarEntry{name: "/etc/passwd", body: "x"}},
		{"a symlink", tarEntry{name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
		{"a hardlink", tarEntry{name: "hard", typeflag: tar.TypeLink, linkname: "mod.manifest.json"}},
		{"a character device", tarEntry{name: "dev", typeflag: tar.TypeChar}},
		{"a fifo", tarEntry{name: "pipe", typeflag: tar.TypeFifo}},
		{"a reserved device name", tarEntry{name: "NUL", body: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newEngine(t)
			err := e.ModAction(ModAction{
				ID:           "evil",
				Action:       "install",
				ArchiveBytes: tarZstd(t, []tarEntry{tc.entry}),
			})
			if err == nil {
				t.Fatalf("an archive with %s must be refused", tc.name)
			}
			// The refusal must be a refusal, not a partial unpack: nothing may
			// land beside the mod directory the extractor was given.
			for _, escaped := range []string{"escape.txt", "passwd", "link"} {
				if _, statErr := os.Stat(filepath.Join(e.dirFor("mods"), escaped)); statErr == nil {
					t.Errorf("%s was written outside the mod directory", escaped)
				}
			}
		})
	}
}

// TestModArchiveRefusesAnEmptyPayload pins the one branch a caller reaches by
// accident: an install with no archive bytes is a malformed request, not a
// silent no-op that reports success.
func TestModArchiveRefusesAnEmptyPayload(t *testing.T) {
	e, _ := newEngine(t)
	err := e.ModAction(ModAction{ID: "empty", Action: "install"})
	if err == nil {
		t.Fatal("an install with no archive bytes must be refused")
	}
}

// TestModArchiveDoesNotStripAWrappedTopLevelDirectory records the layout's one
// sharp edge: taring the *folder* instead of its contents nests the whole mod
// one level too deep, so `data/mods/<id>/mod.manifest.json` never appears and
// the mod installs but does nothing. The writer must tar the contents; the
// refusal that would catch this is recorded in §27.5 as a follow-up, because it
// is a behaviour change rather than a clarification.
func TestModArchiveDoesNotStripAWrappedTopLevelDirectory(t *testing.T) {
	e, _ := newEngine(t)
	archive := tarZstd(t, []tarEntry{
		{name: "noir-overhaul/mod.manifest.json", body: `{"id":"noir-overhaul"}`},
	})
	if err := e.ModAction(ModAction{
		ID:           "noir-overhaul",
		Action:       "install",
		ArchiveBytes: archive,
	}); err != nil {
		t.Fatalf("the extractor accepts it; the layout is what makes it wrong: %v", err)
	}
	root := filepath.Join(e.dirFor("mods"), "noir-overhaul")
	if _, err := os.Stat(filepath.Join(root, "mod.manifest.json")); err == nil {
		t.Fatal("a wrapped archive must not land its manifest at the mod root")
	}
	if _, err := os.Stat(filepath.Join(root, "noir-overhaul", "mod.manifest.json")); err != nil {
		t.Fatalf("the wrapper directory is preserved as a nested directory: %v", err)
	}
}

// TestExtractModArchiveFromTheGameCLI reads an archive produced by the game's
// `tools/pack.mjs` through the launcher's own extractor.
//
// `04:04.7` asks for exactly this: a test that runs the writer's output through
// the launcher's reader, so the two agree by test rather than by hope. The path
// comes from the environment and the test skips without it, which keeps
// `make -C launcher test` standalone; the game's `make mods` sets it.
func TestExtractModArchiveFromTheGameCLI(t *testing.T) {
	path := os.Getenv("WSP_MOD_ARCHIVE")
	if path == "" {
		t.Skip("set WSP_MOD_ARCHIVE to an archive built by the game's tools/pack.mjs")
	}
	archive, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}
	dest := t.TempDir()
	if err := extractTarZstd(archive, dest); err != nil {
		t.Fatalf("the game's packer produced an archive the launcher cannot extract: %v", err)
	}
	// The layout §27.5 fixes: the mod's tree at the root, with no wrapper.
	if _, err := os.Stat(filepath.Join(dest, "mod.manifest.json")); err != nil {
		t.Fatalf("the layout must put mod.manifest.json at the archive root: %v", err)
	}
	assets := filepath.Join(dest, "assets")
	info, err := os.Stat(assets)
	if err != nil || !info.IsDir() {
		t.Fatalf("the layout must put the mod tree under assets/: %v", err)
	}
}
