//go:build !linux && !windows && !darwin

package browser

// InstallGuidance returns the generic §20.4 guidance for platforms without a
// specific install path. The string contains no filesystem path, so it is safe
// to print or to show in a dialog (§21.4, §22.4).
func InstallGuidance() string {
	return "No supported browser was found. Kobra Games needs a Chromium-based browser: " +
		"Google Chrome, Microsoft Edge, Brave, Opera, or Chromium. " +
		"Install one with your operating system's package manager, or download it from " +
		"the vendor's website."
}
