// Package paths resolves the game folder and the sidecar location
// (Launcher spec §5, §6.1).
//
// FR-LNCH-1 is load-bearing: the game folder is derived exclusively from the
// launcher's own executable path, never from the working directory, $PWD, or an
// environment variable. A Root is computed once at startup, is immutable
// afterwards, and is the only source of filesystem roots for every other
// package.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ConfigRelPath is the config file's location relative to the game folder.
const ConfigRelPath = "launcher/launcher.config.json"

// Root is the canonical set of filesystem roots for one launcher run.
type Root struct {
	GameFolder  string // absolute, canonical game folder
	GameDir     string // <GameFolder>/game
	DataDir     string // <GameFolder>/data, or the --data-dir override
	LauncherDir string // <GameFolder>/launcher
	SidecarDir  string // OS-specific machine-local state (§6.1)
	LogDir      string // <SidecarDir>/logs
	ConfigPath  string // <GameFolder>/launcher/launcher.config.json

	// DataDirKind is "game" for the in-folder data directory or "override"
	// when --data-dir was supplied (FR-SAVE-19).
	DataDirKind string
	// FolderKind records how the game folder was found: "canonical" when the
	// executable sat directly in <root>/launcher, "nested" when the launcher
	// searched one directory upward (§5.1).
	FolderKind string
	// SidecarKind is "native" or "fallback" (§6.3).
	SidecarKind string
	// SidecarFallbackReason is the plain-language reason for a fallback.
	SidecarFallbackReason string
}

// Sentinel errors for the §5.2 failure modes.
var (
	ErrExecutable    = errors.New("could not determine the launcher's location")
	ErrConfigMissing = errors.New("launcher.config.json not found")
	ErrGameFilesMiss = errors.New("game/index.html not found")
)

// UserMessage maps a resolution failure to the exact user-facing sentence from
// §5.2. It never contains a filesystem path (§22.4).
func UserMessage(err error) string {
	switch {
	case errors.Is(err, ErrExecutable):
		return "Couldn't determine where the launcher is running from. Try moving the game folder to a local drive."
	case errors.Is(err, ErrConfigMissing):
		return "The launcher's configuration file is missing. Re-extract the game archive."
	case errors.Is(err, ErrGameFilesMiss):
		return "The game files are missing. Re-extract the game archive, or apply the latest update."
	default:
		return "The launcher could not start."
	}
}

// Resolve derives the Root from the launcher's executable path.
//
//	dataDirOverride may be "" (use <GameFolder>/data) or an absolute/relative
//	path supplied via --data-dir.
//
//	dataRootForSidecar is "" normally; the caller may pass a pre-validated
//	game-id so the sidecar location can be computed. It is deliberately a
//	separate step from Resolve so that game_id validation (which needs the
//	config) happens before any sidecar directory is created.
func Resolve(execPath, dataDirOverride string) (Root, error) {
	var r Root
	if execPath == "" {
		return r, ErrExecutable
	}
	abs, err := filepath.Abs(execPath)
	if err != nil {
		return r, fmt.Errorf("%w: %v", ErrExecutable, err)
	}
	// §5.2: a symlinked launcher resolves to its target's game folder.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	exeDir := filepath.Dir(abs)
	root := filepath.Dir(exeDir)

	// Step 4/5 of §5.1: verify, then search upward by at most one directory.
	kind := "canonical"
	if !looksLikeGameFolder(root) {
		if parent := filepath.Dir(root); parent != root && looksLikeGameFolder(parent) {
			root = parent
			kind = "nested"
		}
	}
	return buildRoot(root, kind, dataDirOverride)
}

// ResolveWithOverride is the development-only counterpart of Resolve: it takes
// the game folder from an explicit path instead of deriving it from the
// executable.
//
// It exists so a game being developed in a different folder can be served
// without copying files. FR-LNCH-1 forbids deriving the game folder from
// ambient state, and this function does not do that — the path is an explicit
// operator argument — but it is still a weaker guarantee than Resolve, so it is
// reachable only from a build that sets the kobra_dev tag (cmd/kobra-launcher
// registers the flag under that tag). A release binary has no way to call it.
//
// The config and game/ preconditions of §5.1 steps 4-5 are enforced exactly as
// Resolve enforces them.
func ResolveWithOverride(gameFolderOverride, dataDirOverride string) (Root, error) {
	var r Root
	if strings.TrimSpace(gameFolderOverride) == "" {
		return r, fmt.Errorf("no game folder override was supplied")
	}
	abs, err := filepath.Abs(gameFolderOverride)
	if err != nil {
		return r, fmt.Errorf("%w: %v", ErrConfigMissing, err)
	}
	// A dev tree is routinely reached through a symlink, so resolve it the same
	// way §5.2 resolves the executable.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return buildRoot(abs, "override", dataDirOverride)
}

// buildRoot fills in a Root for an already-decided game folder and applies the
// §5.1 verification steps. Both Resolve and ResolveWithOverride go through it,
// so the "config missing" / "game files missing" contract cannot drift between
// them.
func buildRoot(root, kind, dataDirOverride string) (Root, error) {
	var r Root

	// Distinguish "config missing" from "game files missing" so the user
	// message names the actual problem.
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(ConfigRelPath))); err != nil {
		return r, fmt.Errorf("%w: expected %s", ErrConfigMissing, ConfigRelPath)
	}
	gameDir := filepath.Join(root, "game")
	if st, err := os.Stat(filepath.Join(gameDir, "index.html")); err != nil || st.IsDir() {
		return r, ErrGameFilesMiss
	}

	r.GameFolder = root
	r.GameDir = gameDir
	r.LauncherDir = filepath.Join(root, "launcher")
	r.ConfigPath = filepath.Join(root, filepath.FromSlash(ConfigRelPath))
	r.FolderKind = kind
	r.DataDir = filepath.Join(root, "data")
	r.DataDirKind = "game"
	if strings.TrimSpace(dataDirOverride) != "" {
		d, err := filepath.Abs(dataDirOverride)
		if err != nil {
			return r, fmt.Errorf("--data-dir could not be resolved: %w", err)
		}
		r.DataDir = filepath.Clean(d)
		r.DataDirKind = "override"
	}
	return r, nil
}

// looksLikeGameFolder reports whether dir holds both a launcher config and a
// game subdirectory (§5.1 steps 4-5).
func looksLikeGameFolder(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(ConfigRelPath))); err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, "game"))
	return err == nil && st.IsDir()
}

// SidecarDirFor returns the OS-native sidecar directory for a game id (§6.1).
// The game id must already have been validated by the config schema.
func SidecarDirFor(gameID string) (string, error) {
	if gameID == "" {
		return "", errors.New("empty game id")
	}
	if err := ValidateGameID(gameID); err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("LOCALAPPDATA is not set")
		}
		return filepath.Join(base, "KobraGames", gameID), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "KobraGames", gameID), nil
	default:
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			return filepath.Join(x, "kobra-games", gameID), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "state", "kobra-games", gameID), nil
	}
}

// FallbackSidecarDir is the in-folder sidecar used when the native location
// cannot be created (§6.3).
func FallbackSidecarDir(gameFolder string) string {
	return filepath.Join(gameFolder, ".kobra")
}

// SetSidecar installs the sidecar and log directories on the Root and records
// whether the native location or the fallback is in use.
//
// nativeDir is the OS-native path (from SidecarDirFor); fallbackDir is used
// when nativeDir cannot be created. An error means neither location is
// writable, which is a startup failure (§2.5 exit code 1).
func (r *Root) SetSidecar(nativeDir, fallbackDir string) error {
	if dirWritable(nativeDir) {
		r.SidecarDir = nativeDir
		r.LogDir = filepath.Join(nativeDir, "logs")
		r.SidecarKind = "native"
		return nil
	}
	if fallbackDir != "" && dirWritable(fallbackDir) {
		r.SidecarDir = fallbackDir
		r.LogDir = filepath.Join(fallbackDir, "logs")
		r.SidecarKind = "fallback"
		if r.SidecarFallbackReason == "" {
			r.SidecarFallbackReason = "The machine-local state folder is not writable, so the launcher is keeping its state inside the game folder."
		}
		return nil
	}
	return fmt.Errorf("neither the machine-local state folder nor the in-folder fallback is writable")
}

// SetSidecarFallbackReason records a plain-language reason for a fallback.
func (r *Root) SetSidecarFallbackReason(reason string) {
	r.SidecarFallbackReason = reason
}

// dirWritable reports whether dir exists (or can be created) and accepts a
// probe file.
func dirWritable(dir string) bool {
	if dir == "" {
		return false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".writable-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// Confine reports whether full is inside root. It is the shared
// implementation of the confinement check used by static serving (§13.2) and
// the storage engine (§16.4).
func Confine(root, full string) error {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path escapes root")
	}
	return nil
}

// ReservedName reports whether one path segment names a Windows device and
// therefore cannot be used as a file or directory name.
//
// The Windows rule is not "the whole name equals CON": CreateFile resolves a
// name against the device namespace after stripping the extension from the
// FIRST dot and dropping trailing spaces, so con, con.txt, con., and "CON " all
// open the console device, and nul.json discards writes (NUL). Writing a save
// slot called "con" therefore does not create a file — on Windows it can fail
// or silently lose the save — which is why both the storage engine and static
// serving must reject these segments.
//
// The comparison is case-insensitive. Reserved are CON, NUL, AUX, PRN,
// COM1..COM9 and LPT1..LPT9; COM0, COM10, LPT0 and LPT10 are ordinary names, as
// are console and auxiliary. A segment made only of dots and spaces is also
// rejected, because Windows strips it and a name like "..." can collapse to the
// current directory. An empty segment is not reserved: path splitting produces
// one for every absolute path, and it is not a name at all.
func ReservedName(seg string) bool {
	if seg == "" {
		return false
	}
	// A segment of nothing but dots and spaces is stripped by Windows and can
	// collapse to "" or "."; treat it as unusable rather than let it through.
	onlyDotsAndSpaces := true
	for i := 0; i < len(seg); i++ {
		if seg[i] != '.' && seg[i] != ' ' {
			onlyDotsAndSpaces = false
			break
		}
	}
	if onlyDotsAndSpaces {
		return true
	}
	base := seg
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "NUL", "AUX", "PRN":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}
