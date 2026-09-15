//go:build darwin

package browser

import (
	"os"
	"path/filepath"
)

// macOS detection follows §20.1: the application bundles under /Applications and
// ~/Applications, then $PATH.
//
// Deviation from §20.1, documented as required: the version is read with
// `<exe> --version` rather than from the bundle's Info.plist. Reading
// CFBundleShortVersionString would require a plist parser and, more importantly,
// the bundle version can lag the framework version that determines rendering
// behaviour; the binary's own report is the same source Linux uses, so behaviour
// is uniform across platforms. A bundle whose version cannot be determined is
// unsupported and is never launched (§20.1). See readVersion in browser.go.
func init() {
	bases := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		bases = append(bases, filepath.Join(home, "Applications"))
	}

	exact := map[string][]string{}
	add := func(name, bundle, binary string) {
		for _, base := range bases {
			exact[name] = append(exact[name], filepath.Join(base, bundle, "Contents", "MacOS", binary))
		}
	}
	add("chrome", "Google Chrome.app", "Google Chrome")
	add("msedge", "Microsoft Edge.app", "Microsoft Edge")
	add("brave", "Brave Browser.app", "Brave Browser")
	add("opera", "Opera.app", "Opera")
	add("chromium", "Chromium.app", "Chromium")

	plan = searchPlan{
		exact: exact,
		names: map[string][]string{
			"chrome":   {"google-chrome"},
			"msedge":   {"microsoft-edge"},
			"brave":    {"brave-browser"},
			"opera":    {"opera"},
			"chromium": {"chromium"},
		},
	}
}
