//go:build !linux && !windows && !darwin

package browser

// Other Unix-like platforms (the BSDs, illumos) get a best-effort $PATH-only
// plan. There is no §20.1 layout to follow for them; the canonical location for
// a browser installed from ports or pkgsrc is on $PATH. Version probing is the
// same `<exe> --version` path used everywhere else.
func init() {
	plan = searchPlan{
		names: map[string][]string{
			"chrome":   {"google-chrome", "google-chrome-stable"},
			"msedge":   {"microsoft-edge", "microsoft-edge-stable"},
			"brave":    {"brave-browser"},
			"opera":    {"opera"},
			"chromium": {"chromium", "chromium-browser"},
		},
	}
}
