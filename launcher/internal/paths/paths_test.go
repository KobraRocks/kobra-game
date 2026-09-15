package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// makeGameFolder creates a synthetic game folder the way §5.1 expects to find
// one: <root>/launcher/launcher.config.json and <root>/game/index.html.
func makeGameFolder(t *testing.T, nested bool) (root, exe string) {
	t.Helper()
	base := t.TempDir()
	root = base
	if nested {
		root = filepath.Join(base, "GameName", "GameName")
	}
	if err := os.MkdirAll(filepath.Join(root, "launcher"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "game"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "launcher", "launcher.config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "game", "index.html"), []byte("<html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(root, "launcher", "launcher")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, exe
}

func TestResolveCanonicalLayout(t *testing.T) {
	root, exe := makeGameFolder(t, false)
	r, err := Resolve(exe, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.GameFolder != root {
		t.Errorf("GameFolder = %q, want %q", r.GameFolder, root)
	}
	if r.GameDir != filepath.Join(root, "game") {
		t.Errorf("GameDir = %q", r.GameDir)
	}
	if r.DataDir != filepath.Join(root, "data") {
		t.Errorf("DataDir = %q", r.DataDir)
	}
	if r.DataDirKind != "game" {
		t.Errorf("DataDirKind = %q, want game", r.DataDirKind)
	}
	if r.FolderKind != "canonical" {
		t.Errorf("FolderKind = %q, want canonical", r.FolderKind)
	}
	if r.ConfigPath != filepath.Join(root, "launcher", "launcher.config.json") {
		t.Errorf("ConfigPath = %q", r.ConfigPath)
	}
}

// TestResolveSearchesUpOneLevel covers the nested GameName/GameName layout
// §5.1 explicitly accommodates.
func TestResolveSearchesUpOneLevel(t *testing.T) {
	// The real game folder is <base>/GameName; the launcher sits one level
	// deeper at <base>/GameName/GameName/launcher, so filepath.Dir(exeDir)
	// lands on <base>/GameName/GameName and §5.1's one-level upward search
	// finds the real folder.
	base := t.TempDir()
	real := filepath.Join(base, "GameName")
	deep := filepath.Join(real, "GameName", "launcher")
	for _, d := range []string{filepath.Join(real, "launcher"), filepath.Join(real, "game"), deep} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(real, "launcher", "launcher.config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "game", "index.html"), []byte("<html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	nestedExe := filepath.Join(deep, "launcher")
	if err := os.WriteFile(nestedExe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Resolve(nestedExe, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.GameFolder != real {
		t.Errorf("GameFolder = %q, want %q", r.GameFolder, real)
	}
	if r.FolderKind != "nested" {
		t.Errorf("FolderKind = %q, want nested", r.FolderKind)
	}
}

func TestResolveFailsWhenConfigMissing(t *testing.T) {
	_, exe := makeGameFolder(t, false)
	if err := os.Remove(filepath.Join(filepath.Dir(exe), "launcher.config.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(exe, ""); err == nil {
		t.Fatal("expected an error when the config is missing")
	} else if UserMessage(err) == "" {
		t.Error("no user message for a missing config")
	}
}

func TestResolveFailsWhenGameFilesMissing(t *testing.T) {
	root, exe := makeGameFolder(t, false)
	if err := os.Remove(filepath.Join(root, "game", "index.html")); err != nil {
		t.Fatal(err)
	}
	_, err := Resolve(exe, "")
	if err == nil {
		t.Fatal("expected an error when game/index.html is missing")
	}
	msg := UserMessage(err)
	if msg == "" {
		t.Fatal("no user message")
	}
	// §5.2 names the condition, and §22.4 forbids paths in user messages.
	if len(msg) == 0 || filepath.IsAbs(msg) {
		t.Errorf("message looks wrong: %q", msg)
	}
}

// TestResolveNeverUsesWorkingDirectory is the load-bearing FR-LNCH-1 property:
// the game folder comes from the executable path and nothing else.
func TestResolveNeverUsesWorkingDirectory(t *testing.T) {
	root, exe := makeGameFolder(t, false)
	other := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()

	r, err := Resolve(exe, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.GameFolder != root {
		t.Errorf("GameFolder = %q, want %q (must not depend on cwd)", r.GameFolder, root)
	}
}

func TestResolveDataDirOverride(t *testing.T) {
	root, exe := makeGameFolder(t, false)
	override := t.TempDir()
	r, err := Resolve(exe, override)
	if err != nil {
		t.Fatal(err)
	}
	if r.DataDir != override {
		t.Errorf("DataDir = %q, want the override %q", r.DataDir, override)
	}
	if r.DataDirKind != "override" {
		t.Errorf("DataDirKind = %q, want override", r.DataDirKind)
	}
	// The game folder is unaffected by the override.
	if r.GameFolder != root {
		t.Errorf("GameFolder changed with --data-dir: %q", r.GameFolder)
	}
}

// TestResolveFollowsSymlinkedLauncher documents §5.2's known behaviour: a
// symlinked launcher resolves to its target's game folder.
func TestResolveFollowsSymlinkedLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root, exe := makeGameFolder(t, false)
	linkDir := t.TempDir()
	link := filepath.Join(linkDir, "launcher")
	if err := os.Symlink(exe, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	r, err := Resolve(link, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.GameFolder != root {
		t.Errorf("GameFolder = %q, want the target's folder %q", r.GameFolder, root)
	}
}

func TestValidateGameID(t *testing.T) {
	good := []string{"com.kobra.stardrifter", "a.b", "com.example.my-game", "x.y2.z3"}
	for _, id := range good {
		if err := ValidateGameID(id); err != nil {
			t.Errorf("ValidateGameID(%q) = %v, want nil", id, err)
		}
	}
	// Note: a trailing hyphen ("com.kobra-") is *accepted* because the
	// normative schema pattern is ^[a-z0-9]+(\.[a-z0-9-]+)+$ — the segment
	// charset includes "-" with no trailing-character restriction. This
	// implementation follows the schema rather than being stricter than the
	// contract.
	bad := []string{"", "noseparator", "Com.Kobra", "com.kobra.", ".com.kobra", "com..kobra",
		"com/kobra", "com.kobra_x", "com.kobra."}
	for _, id := range bad {
		if err := ValidateGameID(id); err == nil {
			t.Errorf("ValidateGameID(%q) = nil, want error", id)
		}
	}
}

func TestConfine(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "a", "b.txt")
	if err := Confine(root, inside); err != nil {
		t.Errorf("Confine rejected a path inside root: %v", err)
	}
	outside := filepath.Join(root, "..", "escape.txt")
	if err := Confine(root, outside); err == nil {
		t.Errorf("Confine accepted a path outside root")
	}
	if err := Confine(root, filepath.Join(root, "a", "..", "..", "escape")); err == nil {
		t.Errorf("Confine accepted a dot-segment escape")
	}
}

// TestReservedName pins the one Windows device-name rule. The extension-bearing
// and trailing-dot/space cases are the ones a whole-string comparison misses,
// and con/nul are the ones that silently lose a save on Windows.
func TestReservedName(t *testing.T) {
	reserved := []string{
		"con", "CON", "con.txt", "nul.json", "aux.bak", "prn.dat",
		"com1", "lpt9", "con.", "CON ", "nul.txt", "...", " ",
	}
	for _, seg := range reserved {
		if !ReservedName(seg) {
			t.Errorf("ReservedName(%q) = false, want true", seg)
		}
	}
	allowed := []string{
		"lpt0", "console", "com0", "com10", "lpt10", "auxiliary",
		"tex.png", "slot-1", "",
	}
	for _, seg := range allowed {
		if ReservedName(seg) {
			t.Errorf("ReservedName(%q) = true, want false", seg)
		}
	}
}

func TestSetSidecarFallbackWhenNativeUnwritable(t *testing.T) {
	root, _ := makeGameFolder(t, false)
	r, err := Resolve(filepath.Join(root, "launcher", "launcher"), "")
	if err != nil {
		t.Fatal(err)
	}
	// A native path that cannot be created (a file sits where a directory
	// must go) forces the in-folder fallback of §6.3.
	blocker := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(blocker, "kobra-games", "com.kobra.test")
	if err := r.SetSidecar(native, FallbackSidecarDir(r.GameFolder)); err != nil {
		t.Fatalf("fallback should have succeeded: %v", err)
	}
	if r.SidecarKind != "fallback" {
		t.Errorf("SidecarKind = %q, want fallback", r.SidecarKind)
	}
	if r.SidecarDir != FallbackSidecarDir(r.GameFolder) {
		t.Errorf("SidecarDir = %q, want the in-folder fallback", r.SidecarDir)
	}
	if r.LogDir == "" {
		t.Error("LogDir not set on the fallback path")
	}
}

func TestSetSidecarNativeWhenWritable(t *testing.T) {
	root, _ := makeGameFolder(t, false)
	r, _ := Resolve(filepath.Join(root, "launcher", "launcher"), "")
	native := filepath.Join(t.TempDir(), "state", "com.kobra.test")
	if err := r.SetSidecar(native, FallbackSidecarDir(r.GameFolder)); err != nil {
		t.Fatal(err)
	}
	if r.SidecarKind != "native" {
		t.Errorf("SidecarKind = %q, want native", r.SidecarKind)
	}
	if r.LogDir != filepath.Join(native, "logs") {
		t.Errorf("LogDir = %q", r.LogDir)
	}
}

func TestSidecarDirForUsesStateHomeOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG_STATE_HOME is a Linux convention")
	}
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	got, err := SidecarDirFor("com.kobra.stardrifter")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(state, "kobra-games", "com.kobra.stardrifter")
	if got != want {
		t.Errorf("SidecarDirFor = %q, want %q", got, want)
	}
	// A game id that is not reverse-DNS must never become a directory name.
	if _, err := SidecarDirFor("../escape"); err == nil {
		t.Error("SidecarDirFor accepted an unsafe game id")
	}
}
