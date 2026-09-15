//go:build windows

package browser

import (
	"os"
	"path/filepath"
)

// Windows detection follows §20.1's canonical install locations under
// %ProgramFiles%, %ProgramFiles(x86)% and %LOCALAPPDATA%, then $PATH.
//
// Two deliberate deviations from §20.1 are documented here:
//
//  1. App Paths registry lookup is not implemented. §20.1 names
//     golang.org/x/sys/windows/registry, and that package does exist in this
//     sandbox's module cache, but the launcher module does not require
//     golang.org/x/sys and has no go.sum entry for it. Adding the dependency
//     would mean editing launcher/go.mod and launcher/go.sum, which are outside
//     this package's permitted working directory, so registry lookup is deferred.
//     The canonical locations plus $PATH cover a default install; a browser in a
//     non-default location is still reachable through the `--browser <path>`
//     CLI flag (§B.1) or by putting it on $PATH.
//
//  2. Version is read with `<exe> --version` instead of the executable's Win32
//     file-version resource, which would need version.dll or
//     golang.org/x/sys/windows. §20.1 allows the fallback: a browser whose
//     version cannot be determined is treated as unsupported and is never
//     launched. See readVersion in browser.go.
func init() {
	exact := map[string][]string{}
	bases := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("LOCALAPPDATA"),
	}
	add := func(name string, relPaths ...string) {
		for _, base := range bases {
			if base == "" {
				continue
			}
			for _, rel := range relPaths {
				exact[name] = append(exact[name], filepath.Join(base, filepath.FromSlash(rel)))
			}
		}
	}
	add("chrome", "Google/Chrome/Application/chrome.exe")
	add("msedge", "Microsoft/Edge/Application/msedge.exe")
	add("brave",
		"BraveSoftware/Brave-Browser/Application/brave.exe",
		"Programs/BraveSoftware/Brave-Browser/Application/brave.exe")
	add("opera",
		"Programs/Opera/opera.exe",
		"Opera/opera.exe",
		"Programs/Opera GX/opera.exe")
	add("chromium",
		"Chromium/Application/chrome.exe",
		"Programs/Chromium/Application/chrome.exe")

	plan = searchPlan{
		exact: exact,
		// exec.LookPath applies PATHEXT, so the bare names are enough for $PATH.
		names: map[string][]string{
			"chrome":   {"chrome"},
			"msedge":   {"msedge"},
			"brave":    {"brave"},
			"opera":    {"opera"},
			"chromium": {"chromium"},
		},
	}
}
