//go:build darwin

package browser

// InstallGuidance returns the §20.4 macOS guidance: install one of the supported
// browsers from the Mac App Store where available, or from the vendor's website.
// The string contains no filesystem path, so it is safe to print or to show in a
// dialog (§21.4, §22.4).
func InstallGuidance() string {
	return "No supported browser was found. Kobra Games needs a Chromium-based browser: " +
		"Google Chrome, Microsoft Edge, Brave, Opera, or Chromium. " +
		"Install one from the Mac App Store where available, or download Google Chrome, " +
		"Brave, Opera, or Chromium from their websites."
}
