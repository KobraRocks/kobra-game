// Package browser implements candidate browser detection, minimum-version
// gating, and process launch for the launcher (Launcher spec §20, §4 step 12,
// §2.5; Appendices A.8 and C.6).
//
// The package never reads or writes launcher configuration, never contacts the
// network, and never executes a browser except to read its version
// (`<exe> --version`, §20.2). Launch is fire-and-forget: the browser outlives
// the launcher (§20.3).
//
// Privacy rule (§21.4, §22.4): an executable path is never logged and never
// placed in an error message. Candidate.Path exists for the caller (the process
// that must be started); the `browser.detected` and `browser.launch` events in
// Appendix C.6 carry the browser name and version only.
//
// Detection sources differ per OS (see detect_*.go). XDG desktop-entry parsing
// on Linux and App Paths registry lookup on Windows are deliberately not
// implemented; the reasons are documented at those call sites.
package browser

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/diagnostics"
)

// Candidate is one detected, version-compatible browser (§20.1, §20.2).
type Candidate struct {
	Name    string // canonical: "chrome", "msedge", "brave", "opera", "chromium"
	Path    string // executable path (never logged, never put in an error)
	Version string // full version string, e.g. "117.0.5938.132"
	Major   int    // parsed major version
}

var (
	// ErrNoBrowser reports that no candidate executable was found at all.
	// Callers surface InstallGuidance and exit 1 (§2.5, §20.4).
	ErrNoBrowser = errors.New("browser: no candidate browser found")

	// ErrTooOld reports that candidate executables exist but none meets the
	// configured minimum, or none reports a usable version — §20.1 treats an
	// undeterminable version as unsupported. The launcher does not launch
	// such a browser (FR-COMPAT-1), and exits 1.
	ErrTooOld = errors.New("browser: no candidate meets the minimum version")
)

// versionMetadataTimeout bounds a single `<exe> --version` probe (§20.2). A
// browser that does not answer in time is unsupported, not launched.
const versionMetadataTimeout = 3 * time.Second

// defaultPreference is the probe order used when browser_preference is empty.
// It mirrors the config schema's default (`chrome, msedge, brave, opera`) and
// appends chromium, which the schema also allows (§20.1). The preference list
// is authoritative: a browser absent from it is never returned, even if
// installed.
var defaultPreference = []string{"chrome", "msedge", "brave", "opera", "chromium"}

// canonicalNames is the set of names this package can probe. The config schema
// also permits vivaldi, firefox and custom; those targets are unsupported
// (§15.1), so a preference entry naming one is skipped rather than guessed at.
var canonicalNames = []string{"chrome", "msedge", "brave", "opera", "chromium"}

// aliasToCanonical accepts the spellings a config author might reasonably use.
var aliasToCanonical = map[string]string{
	"chrome":           "chrome",
	"google-chrome":    "chrome",
	"googlechrome":     "chrome",
	"msedge":           "msedge",
	"edge":             "msedge",
	"microsoft-edge":   "msedge",
	"brave":            "brave",
	"brave-browser":    "brave",
	"opera":            "opera",
	"chromium":         "chromium",
	"chromium-browser": "chromium",
}

// searchPlan is the per-OS, per-candidate lookup recipe (§20.1). It is a
// variable so tests can point detection at a temporary directory; tests that
// touch it must not run in parallel.
type searchPlan struct {
	// dirs are searched in order for each bare name in names, e.g.
	// /usr/bin + google-chrome -> /usr/bin/google-chrome.
	dirs []string
	// exact holds absolute paths probed verbatim, per canonical name, ahead of
	// dirs. It carries the install layouts that do not fit a dir+name pair
	// (/opt/google/chrome/chrome, the Windows per-vendor trees, and the macOS
	// .app bundles).
	exact map[string][]string
	// flatpakDirs are the Linux flatpak export directories; flatpakIDs holds
	// the reverse-DNS ids to look for there. Both are empty off Linux.
	flatpakDirs []string
	flatpakIDs  map[string][]string
	// names are the bare executable names resolved against dirs and then $PATH.
	names map[string][]string
}

// plan is populated by init in the per-OS detect_*.go files.
var plan searchPlan

// Detect probes candidates in pref order and returns those at or above the
// minimum major version (mapped per browser by minimumMajor). The result keeps
// pref order, is deduplicated, and never contains a browser below the minimum or
// one whose version cannot be determined (§20.1, §20.2).
//
// The error is non-nil only when nothing acceptable exists. It wraps
// ErrNoBrowser when no candidate executable was found at all, and ErrTooOld when
// executables exist but all are too old or version-less, so callers can use
// errors.Is. Neither error message contains a filesystem path (§22.4).
//
// Detect must not be used as a substitute for launching: it runs a browser only
// to read its version (§20.2, §20.4).
func Detect(pref []string, min config.MinBrowserVersion) ([]Candidate, error) {
	accepted := make([]Candidate, 0, len(pref))
	foundExecutable := false

	// probeOrder has already de-duplicated and normalised the preference list.
	for _, name := range probeOrder(pref) {
		exe, ok := findExecutable(name)
		if !ok {
			continue
		}
		foundExecutable = true

		version, major, ok := readVersion(exe)
		if !ok {
			// §20.1: an undeterminable version is unsupported and is never
			// launched. No event is emitted for a skip (Appendix C.6 has no
			// event for it) and, deliberately, no path is logged.
			continue
		}
		if floor, hasFloor := minimumMajor(name, min); hasFloor && major < floor {
			continue
		}

		accepted = append(accepted, Candidate{
			Name:    name,
			Path:    exe,
			Version: version,
			Major:   major,
		})
		logEvent(diagnostics.LevelInfo, "browser.detected", map[string]any{
			"name":    name,
			"version": version,
		})
	}

	if len(accepted) > 0 {
		return accepted, nil
	}
	if !foundExecutable {
		return nil, fmt.Errorf("%w", ErrNoBrowser)
	}
	return nil, fmt.Errorf("every detected browser is below the minimum version or reports no version: %w", ErrTooOld)
}

// probeOrder normalises pref into canonical names in the caller's order. The
// preference list is authoritative, so nothing is appended: a browser absent
// from it is never returned, and a pref that lists only unsupported targets
// (vivaldi, firefox, custom — §15.1) yields nothing and therefore ErrNoBrowser.
// Only an empty pref falls back to defaultPreference, the config schema's
// default order.
func probeOrder(pref []string) []string {
	if len(pref) == 0 {
		return append([]string(nil), defaultPreference...)
	}
	out := make([]string, 0, len(pref))
	for _, raw := range pref {
		canonical, ok := aliasToCanonical[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			continue
		}
		duplicate := false
		for _, existing := range out {
			if existing == canonical {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, canonical)
		}
	}
	return out
}

// findExecutable resolves one canonical name to an executable, in the §20.1
// order: absolute well-known paths, then $PATH. It stats files only; it never
// executes anything.
func findExecutable(name string) (string, bool) {
	if exe, ok := firstExecutable(plan.exact[name]); ok {
		return exe, true
	}
	for _, dir := range plan.dirs {
		for _, bare := range plan.names[name] {
			if exe, ok := firstExecutable([]string{filepath.Join(dir, bare)}); ok {
				return exe, true
			}
		}
	}
	for _, dir := range plan.flatpakDirs {
		for _, id := range plan.flatpakIDs[name] {
			if exe, ok := firstExecutable([]string{filepath.Join(dir, id)}); ok {
				return exe, true
			}
		}
	}
	for _, bare := range plan.names[name] {
		if exe, err := exec.LookPath(bare); err == nil {
			if isExecutableFile(exe) {
				return exe, true
			}
		}
	}
	return "", false
}

func firstExecutable(paths []string) (string, bool) {
	for _, p := range paths {
		if p != "" && isExecutableFile(p) {
			return p, true
		}
	}
	return "", false
}

// versionRe matches the first dotted version group in --version output. The
// first three components are mandatory and a fourth is consumed when present, so
// the returned string is the full version ("117.0.5938.132"). Anchoring on the
// first match is what makes Brave work: `brave-browser --version` prints
// "Brave Browser 1.45.116 Chromium: 117.0.5938.132", and the Brave version is
// the one that matters for the Brave floor, not the bundled Chromium one.
var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:\.\d+)*`)

// parseVersion extracts the first full version group and its major component
// from `--version` output (stdout or stderr, which browsers disagree about).
func parseVersion(out string) (version string, major int, ok bool) {
	m := versionRe.FindString(out)
	if m == "" {
		return "", 0, false
	}
	first := m
	if i := strings.IndexByte(first, '.'); i >= 0 {
		first = first[:i]
	}
	n, err := strconv.Atoi(first)
	if err != nil {
		return "", 0, false
	}
	return m, n, true
}

// readVersion runs `<exe> --version` with a 3-second timeout and parses the
// first version group from its combined output (§20.2).
//
// §20.1 specifies reading version metadata from the executable on Windows and
// macOS. Both currently fall back to `--version` so that behaviour is uniform
// and the package needs no platform SDK or extra module dependency; the
// deviations are documented in detect_windows.go and detect_darwin.go. A browser
// whose version still cannot be determined is unsupported and is never launched
// (§20.1).
func readVersion(exe string) (string, int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), versionMetadataTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "--version")
	// The exit status is deliberately ignored: some wrappers (notably snap and
	// flatpak shims) print the version and then exit non-zero, and the error
	// value would carry the executable path. Only empty output is fatal.
	out, _ := cmd.CombinedOutput()
	if len(out) == 0 {
		// Empty output (including a timeout kill) means the version cannot be
		// determined; a version-less browser is unsupported (§20.1).
		return "", 0, false
	}
	// The version is usable even when the wrapper exited non-zero.
	return parseVersion(string(out))
}

// minimumMajor maps a canonical candidate name to the configured minimum major
// version. The second result is false when no floor applies.
//
// Mapping (config.MinBrowserVersion has no chromium field):
//   - chrome and chromium both use min.Chrome. Chromium and Chrome share a
//     version train and rendering engine, and §20.2 lists no separate Chromium
//     minimum, so the Chrome floor is the closest correct analogue.
//   - msedge uses min.Edge, opera uses min.Opera.
//   - brave uses the *major component of the string* min.Brave ("1.45" -> 1).
//     This is the documented interpretation for this package: the configured
//     Brave minimum is a string because Brave's marketing version is not a plain
//     integer, and only its major is compared. The practical effect is coarse —
//     any Brave 1.x passes a "1.45" floor — which is why the value is a floor
//     for the major train, not a patch-level gate. Changing that requires a
//     config/schema change, not a change here.
//
// A zero or malformed minimum means "no floor" rather than "reject everything":
// a bad config value must not silently make every browser incompatible.
func minimumMajor(name string, min config.MinBrowserVersion) (int, bool) {
	switch name {
	case "chrome", "chromium":
		return min.Chrome, min.Chrome > 0
	case "msedge":
		return min.Edge, min.Edge > 0
	case "opera":
		return min.Opera, min.Opera > 0
	case "brave":
		if n, ok := parseMajorString(min.Brave); ok {
			return n, true
		}
	}
	return 0, false
}

// parseMajorString returns the major component of a dotted version string:
// "1.45" -> 1, "2" -> 2. A blank or malformed string yields ok=false.
func parseMajorString(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return n, true
}

// distroInstallCommand maps a Linux distribution family (the ID or an ID_LIKE
// token from os-release) to the plain-language install command shown by
// InstallGuidance (§20.4). The command contains no filesystem path. It lives in
// this shared file so the mapping is testable on every platform.
func distroInstallCommand(family string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case "debian", "ubuntu", "linuxmint", "pop", "raspbian", "kali", "elementary", "zorin", "devuan":
		return "sudo apt install chromium", true
	case "fedora", "rhel", "centos", "rocky", "almalinux", "amzn", "ol":
		return "sudo dnf install chromium", true
	case "arch", "manjaro", "endeavouros", "garuda", "cachyos", "artix":
		return "sudo pacman -S chromium", true
	case "opensuse", "opensuse-leap", "opensuse-tumbleweed", "sles", "suse":
		return "sudo zypper install chromium", true
	case "alpine":
		return "sudo apk add chromium", true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Logging
// ---------------------------------------------------------------------------

var (
	logMu sync.RWMutex
	log   *diagnostics.Logger
)

// SetLogger installs the structured logger used for the `browser.detected` and
// `browser.launch` events (Appendix C.6). It is optional: with no logger
// installed, detection and launch are silent. It is safe to call while
// detection or launch is running, and passing nil removes the logger.
func SetLogger(l *diagnostics.Logger) {
	logMu.Lock()
	log = l
	logMu.Unlock()
}

func logEvent(level diagnostics.Level, evt string, fields map[string]any) {
	logMu.RLock()
	l := log
	logMu.RUnlock()
	if l == nil {
		return
	}
	l.Event(level, evt, fields)
}
