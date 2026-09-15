//go:build windows

package browser

// InstallGuidance returns the §20.4 Windows guidance. It names the Microsoft
// Store path for Edge, which ships with Windows, and the vendors' websites for
// the others. The string contains no filesystem path: the caller can print it or
// put it in an OS dialog without revealing the install location (§21.4, §22.4).
func InstallGuidance() string {
	return "No supported browser was found. Kobra Games needs a Chromium-based browser: " +
		"Google Chrome, Microsoft Edge, Brave, Opera, or Chromium. " +
		"Microsoft Edge is included with Windows: open the Microsoft Store and install " +
		"or repair it there. Google Chrome, Brave, and Opera can also be downloaded from " +
		"their websites."
}
