package browser

import (
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"kobragames.local/launcher/internal/diagnostics"
)

// WatchdogTimeout is the §20.5 window between launching the browser and requiring
// the first /__kobra/heartbeat. The heartbeat watcher owns the timer and emits
// `browser.watchdog.timeout` (Appendix C.6) when it fires, once, without
// retrying; this package only publishes the interval so both sides agree on it.
const WatchdogTimeout = 20 * time.Second

// Launch starts the browser at url and returns without waiting for it (§20.3).
//
// It uses exec.Command(...).Start(), never Run: the browser outlives the
// launcher, and the launcher must not block while a page loads. Stdio is not
// inherited — os/exec maps a nil Stdin/Stdout/Stderr to the null device — so a
// GUI browser cannot write to, or read from, the launcher's console.
//
// On Windows the child is started with HideWindow so no console flashes (§20.3).
// POSIX deliberately does not detach with Setsid: the spec's snippet is a plain
// Start, and keeping the browser in the launcher's session preserves normal
// terminal job-control behaviour.
//
// The `browser.launch` event (Appendix C.6) is emitted after a successful start
// and carries the canonical browser name only; the executable path is never
// logged (§22.4).
func Launch(path, url string) error {
	cmd := exec.Command(path, url)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	applySysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return &LaunchError{cause: err}
	}

	logEvent(diagnostics.LevelInfo, "browser.launch", map[string]any{
		"name": nameFromPath(path),
	})

	// Reap the child when it eventually exits so a browser that dies while the
	// launcher is still running does not linger as a zombie. This is not a wait:
	// Launch has already returned and the browser is already running.
	go func() { _ = cmd.Wait() }()

	return nil
}

// LaunchError reports that the browser process could not be started.
//
// Error() is path-free even though the underlying *os.PathError or *exec.Error
// usually embeds the executable path: §22.4 forbids filesystem paths in
// user-facing messages. Cause exposes it for operator diagnostics; callers must
// not print it verbatim or write it into a structured event field.
type LaunchError struct {
	cause error
}

func (e *LaunchError) Error() string { return "browser: could not start the browser process" }

// Cause returns the underlying error, which may contain a filesystem path and
// is therefore not safe to surface to the user or to log as an event field.
func (e *LaunchError) Cause() error { return e.cause }

// nameFromPath maps an executable path to the canonical browser name used by the
// `browser.launch` event (Appendix C.6). The path itself is never logged; a
// binary this package does not recognise — for example one chosen with
// `--browser <path>` — is reported as "unknown" so that no user path fragment
// leaks into the log (§22.4).
func nameFromPath(path string) string {
	base := strings.ToLower(filepath.Base(path))
	switch {
	case strings.Contains(base, "chromium"):
		return "chromium"
	case strings.Contains(base, "chrome"):
		return "chrome"
	case strings.Contains(base, "msedge"), strings.Contains(base, "microsoft-edge"),
		base == "edge", base == "edge.exe":
		return "msedge"
	case strings.Contains(base, "brave"):
		return "brave"
	case strings.Contains(base, "opera"):
		return "opera"
	}
	return "unknown"
}
