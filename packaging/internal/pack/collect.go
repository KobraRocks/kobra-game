package pack

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The launcher's static handler registers an explicit route allowlist
// (launcher/internal/static/static.go): / and /index.html, /shell.js, /engine/,
// /assets/, /locales/ and /editor/. A file under game/ that matches none of
// those is packaged, indexed, and then 404s in the browser. That is a build
// failure.
var (
	gameRootServed = map[string]bool{"index.html": true, "shell.js": true}
	// gameRootRequired is the subset of gameRootServed that must actually
	// exist: §5.4 and §11.2 gate 1 make a package without either file
	// unshippable, and a file that is absent is invisible to a walk that only
	// inspects what is present.
	gameRootRequired = []string{"index.html", "shell.js"}
	// release.manifest.json is exempt: the launcher reads it from disk for the
	// update check and never serves it over HTTP.
	gameRootUnserved = map[string]bool{"release.manifest.json": true}
	// editor/ is optional: a game that ships no editor simply has no such
	// directory, and the route 404s. It is listed here so that an editor the
	// game *does* ship is served rather than packaged-and-404'd.
	servablePrefixes = []string{"engine/", "assets/", "locales/", "editor/"}
)

var (
	vcsDirs    = map[string]bool{".git": true, ".svn": true, ".hg": true, ".bzr": true}
	editorJunk = map[string]bool{".DS_Store": true, "Thumbs.db": true, ".idea": true, ".vscode": true}
	// deviceNames mirrors reservedNames in launcher/internal/update/archive.go.
	// A name this table accepts but the launcher's rejects would ship a package
	// the launcher cannot install, so the two tables MUST be kept in sync by
	// hand: the launcher module and this one cannot import each other.
	// TestReservedDeviceNamesArePinned fails if this set changes unexpectedly.
	deviceNames = func() map[string]bool {
		out := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
		for i := 1; i <= 9; i++ {
			out[fmt.Sprintf("COM%d", i)] = true
			out[fmt.Sprintf("LPT%d", i)] = true
		}
		return out
	}()
)

// CollectTree copies pkgroot/game into dest/game, then applies the release
// overlay on top. The build never writes into pkgroot (§2.2).
func CollectTree(pkgRoot, overlay, dest, gameName string) error {
	src := filepath.Join(pkgRoot, "game")
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		return Fail(EvStart, "pkgroot has no game/ directory", map[string]any{"reason": "no_game"})
	}
	target := filepath.Join(dest, gameName, "game")
	if err := copyTree(src, target); err != nil {
		return Fail(EvStart, "the game tree could not be staged", map[string]any{"reason": "copy"})
	}
	if overlay == "" {
		return nil
	}
	if st, err := os.Stat(overlay); err != nil || !st.IsDir() {
		return Fail(EvStart, "a release overlay directory is missing", map[string]any{"reason": "no_overlay"})
	}
	files, err := RelFiles(overlay)
	if err != nil {
		return Fail(EvStart, "a release overlay could not be read", map[string]any{"reason": "overlay"})
	}
	for _, rel := range files {
		source := filepath.Join(overlay, filepath.FromSlash(rel))
		target := filepath.Join(dest, gameName, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyFile(source, target); err != nil {
			return Fail(EvStart, "an overlay file could not be applied", map[string]any{"path": rel})
		}
	}
	return nil
}

// Problem is one rejected path and the rule it broke.
type Problem struct {
	Path   string
	Reason string
}

// ScanErrors carries every rejected path, so one build reports all of them
// rather than stopping at the first.
type ScanErrors struct {
	Problems []Problem
}

func (e *ScanErrors) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.Path+" ("+p.Reason+")")
	}
	return fmt.Sprintf("%d forbidden path(s): %s", len(parts), strings.Join(parts, "; "))
}

// Emit writes one pack.scan.reject event per problem.
func (e *ScanErrors) Emit(log *Logger) {
	for _, p := range e.Problems {
		log.Error(EvScanReject, map[string]any{"path": p.Path, "reason": p.Reason})
	}
}

// ScanForbidden implements §2.4: it fails the build rather than the user.
func ScanForbidden(stage, gameName string) error {
	var problems []Problem

	root := filepath.Join(stage, gameName)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		name := info.Name()

		if info.Mode()&os.ModeSymlink != 0 {
			problems = append(problems, Problem{rel, "symlink"})
			return nil
		}
		if info.IsDir() {
			if vcsDirs[name] {
				problems = append(problems, Problem{rel, "version-control metadata"})
				return filepath.SkipDir
			}
			if editorJunk[name] {
				problems = append(problems, Problem{rel, "editor metadata"})
				return filepath.SkipDir
			}
			return nil
		}

		if editorJunk[name] || strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".swp") {
			problems = append(problems, Problem{rel, "editor metadata"})
		}
		if strings.HasPrefix(name, ".") && name != ".keep" {
			problems = append(problems, Problem{rel, "hidden file"})
		}
		for _, segment := range strings.Split(rel, "/") {
			stem := strings.ToUpper(strings.SplitN(segment, ".", 2)[0])
			if deviceNames[stem] {
				problems = append(problems, Problem{rel, "windows device name " + segment})
			}
			if strings.TrimRight(segment, ". ") != segment {
				problems = append(problems, Problem{rel, "trailing dot or space"})
			}
		}
		// data/ holds the user's saves; only .keep may ship (§2.5).
		parts := strings.Split(rel, "/")
		if len(parts) >= 2 && parts[0] == "data" && !(len(parts) == 2 && parts[1] == ".keep") {
			problems = append(problems, Problem{rel, "only data/.keep may ship under data/ (FR-UPD-7)"})
		}
		return nil
	})
	if err != nil {
		return Fail(EvScanReject, "the staged tree could not be scanned", map[string]any{"reason": "walk"})
	}

	// launcher/ must hold exactly what §2.3 lists.
	launcherDir := filepath.Join(root, "launcher")
	if st, err := os.Stat(launcherDir); err == nil && st.IsDir() {
		allowed := map[string]bool{
			"launcher": true, "launcher.exe": true, "launcher.config.json": true,
			"port-deny-list.json": true, "VERSION": true,
		}
		entries, _ := os.ReadDir(launcherDir)
		for _, entry := range entries {
			if !allowed[entry.Name()] {
				problems = append(problems, Problem{"launcher/" + entry.Name(), "not part of the §2.3 launcher layout"})
			}
		}
	}

	if len(problems) > 0 {
		sort.Slice(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
		return &ScanErrors{Problems: problems}
	}
	return nil
}

// CheckServable fails when a required root file is absent, or when a file under
// game/ could not be fetched over HTTP.
func CheckServable(stage, gameName string) error {
	gameDir := filepath.Join(stage, gameName, "game")
	files, err := RelFiles(gameDir)
	if err != nil {
		return Fail(EvScanReject, "the game tree could not be listed", map[string]any{"reason": "walk"})
	}
	var problems []Problem

	// §5.4/§11.2: the two root files must exist, not merely be servable if they
	// do. The relative path is named, never the staging directory, so the
	// message is the same in a checkout and in CI.
	for _, required := range gameRootRequired {
		if _, err := os.Stat(filepath.Join(gameDir, required)); err != nil {
			problems = append(problems, Problem{"game/" + required,
				"required by §5.4 but missing"})
		}
	}

	for _, rel := range files {
		if strings.Contains(rel, "/") {
			servable := false
			for _, prefix := range servablePrefixes {
				if strings.HasPrefix(rel, prefix) {
					servable = true
					break
				}
			}
			if !servable {
				problems = append(problems, Problem{"game/" + rel,
					fmt.Sprintf("not under any served prefix %v", servablePrefixes)})
			}
			continue
		}
		if !gameRootServed[rel] && !gameRootUnserved[rel] {
			problems = append(problems, Problem{"game/" + rel,
				"the game root serves only index.html and shell.js; move it under assets/"})
		}
	}
	if len(problems) > 0 {
		sort.Slice(problems, func(i, j int) bool { return problems[i].Path < problems[j].Path })
		return &ScanErrors{Problems: problems}
	}
	return nil
}

// BuildDataSkeleton creates §2.5's empty directories and data/.keep, which must
// be zero bytes and is the only file under data/ in any package.
func BuildDataSkeleton(stage, gameName string) error {
	data := filepath.Join(stage, gameName, "data")
	for _, name := range []string{"saves", "config", "mods"} {
		if err := os.MkdirAll(filepath.Join(data, name), 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(data, ".keep"), nil, 0o644)
}

// Sizes carries the numbers README.txt must state (§10.1).
type Sizes struct {
	Download    int64
	Extracted   int64
	Recommended int64
}

// BuildExtraFiles writes README.txt and LICENSES/, which ship in the ZIP only.
//
// The download size is a fixed-width field on purpose. README.txt lives inside
// the ZIP and states that ZIP's size, so a variable-width number would make the
// size depend on the text that states it. The archive writer stores this one
// file uncompressed, so its contribution is exactly its length and the loop
// settles on the second pass.
func BuildExtraFiles(stage, gameName string, sizes Sizes) error {
	root := filepath.Join(stage, gameName)
	if err := os.MkdirAll(filepath.Join(root, "LICENSES"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSES", "README.txt"), []byte(licenceNotice), 0o644); err != nil {
		return err
	}
	// The notices travel with the launcher binary, which is where the
	// third-party code actually is (see notices.go).
	if err := os.WriteFile(filepath.Join(root, "LICENSES", "THIRD_PARTY_NOTICES.md"), thirdPartyNotices, 0o644); err != nil {
		return err
	}
	readme := fmt.Sprintf(readmeTemplate, gameName, gameName,
		strings.Repeat("=", len(gameName)),
		sizes.Download, sizes.Extracted, sizes.Recommended)
	return os.WriteFile(filepath.Join(root, "README.txt"), []byte(readme), 0o644)
}

// readmeSize is the exact byte length of the README.txt BuildExtraFiles writes
// for a game folder. Every number the template prints is a fixed-width field, so
// the length depends only on the folder name and never on the values.
func readmeSize(gameName string) int64 {
	return int64(len(fmt.Sprintf(readmeTemplate, gameName, gameName,
		strings.Repeat("=", len(gameName)), int64(0), int64(0), int64(0))))
}

const licenceNotice = `This package embeds the Kobra launcher, which is MIT-licensed. The launcher's
source, including its LICENSE, is at https://github.com/KobraRocks/kobra-game.

THIRD_PARTY_NOTICES.md in this directory carries the full licence texts for every
third-party component statically linked into launcher/launcher (BSD-3-Clause and
Apache-2.0 code). Those notices are part of the distribution: keep this directory
with the package when you redistribute it.
`

const readmeTemplate = `%s
%s%s

This is a Kobra game package. Extract it anywhere and run the launcher:

  Linux:    chmod +x launcher/launcher && ./launcher/launcher
  macOS:    ./launcher/launcher
  Windows:  launcher\launcher.exe

Your progress is stored in the data folder beside this file. Copy the whole
folder to move your saves. There is nothing to install and no account.

SIZES
-----
  Download size (this archive): %18d bytes
  Extracted size:               %18d bytes
  Free space recommended:       %18d bytes
  (An update briefly needs room for two copies of game/.)

FILESYSTEM NOTE
---------------
Some filesystems and older extractors cannot handle a file of 4 GiB or more.
The sizes above are exact; check them against the device you are extracting to.
`

// --- filesystem helpers ----------------------------------------------------

// RelFiles lists every regular file under root as sorted POSIX relative paths.
func RelFiles(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// RelDirs lists every directory under root as sorted POSIX relative paths.
func RelDirs(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() || p == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// IsExecutable reports whether the file carries any execute bit.
func IsExecutable(p string) bool {
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	return st.Mode().Perm()&0o111 != 0
}

// ModeString renders the POSIX mode a manifest records.
func ModeString(p string) string {
	if IsExecutable(p) {
		return "755"
	}
	return "644"
}

// WriteJSON writes a document with a trailing newline and stable indentation.
func WriteJSON(path string, doc any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// ReadJSON decodes a document from disk.
func ReadJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, p)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// Recreate the link rather than following it, so ScanForbidden
			// reports "symlink" as the problem. Following it would hide the
			// defect and could copy data from outside the tree (§2.4).
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type in the game tree")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, st.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
