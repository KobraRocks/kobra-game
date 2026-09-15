//go:build !kobra_faultinject

package faultinject

// This is the release form: every seam is inert. The bodies are empty so the
// compiler inlines them away, and Enabled reports false so a test can assert
// that a default build carries no injection machinery that could fire.

// Point does nothing in a release build.
func Point(string) {}

// Err always reports no injected failure in a release build.
func Err(string) error { return nil }

// Enabled reports whether injection is armed. Always false without the tag.
func Enabled() bool { return false }
