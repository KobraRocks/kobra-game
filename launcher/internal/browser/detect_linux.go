//go:build linux

package browser

import (
	"os"
	"path/filepath"
)

// Linux detection sources, in the §20.1 order: absolute well-known paths, the
// flatpak export directories, then $PATH through exec.LookPath.
//
// XDG desktop-entry parsing is deliberately skipped. §20.1 lists it as a source,
// but parsing Exec= lines and resolving %U/%f placeholders is a second, fuzzier
// lookup path that can hand back a shell wrapper rather than a browser binary;
// the directories below already cover the packaged installs that desktop files
// would point at (distro packages, snap, flatpak). If it is ever added, it
// belongs here as a final fallback in this plan, after the absolute paths.
//
// Nothing here touches launcher configuration or the user's config files.
var (
	// linuxBinaryDirs are joined with the bare names per candidate, e.g.
	// /usr/bin/google-chrome, /usr/local/bin/chromium, /snap/bin/chromium.
	linuxBinaryDirs = []string{"/usr/bin", "/usr/local/bin", "/snap/bin"}

	// linuxExactPaths hold install layouts that are not dir+name pairs.
	linuxExactPaths = map[string][]string{
		"chrome": {"/opt/google/chrome/chrome"},
		// Snap/Flatpak shims are covered by linuxBinaryDirs and the flatpak
		// export directories below.
	}

	// linuxFlatpakIDs maps each canonical name to its Flatpak application id
	// (reverse-DNS), from the list in §20.1.
	linuxFlatpakIDs = map[string][]string{
		"chrome":   {"com.google.Chrome"},
		"msedge":   {"com.microsoft.Edge"},
		"brave":    {"com.brave.Browser"},
		"opera":    {"com.opera.Opera"},
		"chromium": {"org.chromium.Chromium"},
	}

	// linuxNames are the bare executable names probed in linuxBinaryDirs and
	// then on $PATH, per candidate.
	linuxNames = map[string][]string{
		"chrome":   {"google-chrome", "google-chrome-stable"},
		"msedge":   {"microsoft-edge", "microsoft-edge-stable"},
		"brave":    {"brave-browser"},
		"opera":    {"opera"},
		"chromium": {"chromium", "chromium-browser"},
	}
)

func init() {
	plan = searchPlan{
		dirs:        linuxBinaryDirs,
		exact:       linuxExactPaths,
		flatpakDirs: linuxFlatpakExportDirs(),
		flatpakIDs:  linuxFlatpakIDs,
		names:       linuxNames,
	}
}

// linuxFlatpakExportDirs returns the system-wide and per-user flatpak binary
// export directories. The flatpak wrappers are not usually on $PATH, so they are
// probed as absolute paths (§20.1). The home directory is resolved at init; an
// unset HOME simply drops the per-user entry.
func linuxFlatpakExportDirs() []string {
	dirs := []string{"/var/lib/flatpak/exports/bin"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "share", "flatpak", "exports", "bin"))
	}
	return dirs
}
