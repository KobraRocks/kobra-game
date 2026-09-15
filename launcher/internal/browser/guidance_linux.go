//go:build linux

package browser

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// InstallGuidance returns the §20.4 Linux guidance: plain language naming the
// detected distribution family's install command. The string contains no
// filesystem path and no slash at all, so it can be printed, copied into a
// support message, or put in an OS dialog without leaking where the launcher
// lives (§21.4, §22.4).
func InstallGuidance() string {
	family := detectDistroFamily()
	if cmd, ok := distroInstallCommand(family); ok {
		return fmt.Sprintf(
			"No supported browser was found. Kobra Games needs a Chromium-based browser: "+
				"Google Chrome, Microsoft Edge, Brave, Opera, or Chromium. "+
				"Detected distribution family: %s. Install one with: %s. "+
				"Chromium is also available from Flathub, and the other browsers can be "+
				"downloaded from their websites.",
			sanitizeFamily(family), cmd)
	}
	if fam := sanitizeFamily(family); fam != "" {
		return fmt.Sprintf(
			"No supported browser was found. Kobra Games needs a Chromium-based browser: "+
				"Google Chrome, Microsoft Edge, Brave, Opera, or Chromium. "+
				"On %s, install Chromium with your distribution's package manager, "+
				"or download one of the other browsers from its website.", fam)
	}
	return "No supported browser was found. Kobra Games needs a Chromium-based browser: " +
		"Google Chrome, Microsoft Edge, Brave, Opera, or Chromium. " +
		"Install Chromium with your distribution's package manager, or download one of " +
		"the other browsers from its website."
}

// detectDistroFamily reads the distribution identity from os-release. That is OS
// metadata, not launcher or user configuration: §20.1's "no config file" rule is
// about browser_preference and friends, which this package never touches. Both
// values below are reduced to a family name before use, and the raw file contents
// never appear in an event, an error or the guidance text (§22.4).
func detectDistroFamily() string {
	for _, p := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		id, like := parseOSRelease(string(b))
		if _, ok := distroInstallCommand(id); ok {
			return id
		}
		for _, field := range strings.Fields(like) {
			if _, ok := distroInstallCommand(field); ok {
				return field
			}
		}
		if id != "" {
			return id
		}
	}
	return detectFamilyFromPackageManager()
}

// parseOSRelease returns the ID and ID_LIKE values of an os-release file,
// honouring comments, quoting and the shell-style `KEY=value` form.
func parseOSRelease(contents string) (id, like string) {
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "ID":
			id = value
		case "ID_LIKE":
			like = value
		}
	}
	return id, like
}

// detectFamilyFromPackageManager is the fallback when os-release is missing or
// unreadable: the first package manager on $PATH identifies the family. It uses
// exec.LookPath, which stats files and never runs them.
func detectFamilyFromPackageManager() string {
	managers := []struct {
		binary string
		family string
	}{
		{"apt-get", "debian"},
		{"dnf", "fedora"},
		{"pacman", "arch"},
		{"zypper", "opensuse"},
		{"apk", "alpine"},
	}
	for _, m := range managers {
		if _, err := exec.LookPath(m.binary); err == nil {
			return m.family
		}
	}
	return ""
}

// sanitizeFamily keeps only characters that can appear in a distribution id, so
// a hostile or corrupt os-release value cannot inject text (or a path) into the
// guidance.
func sanitizeFamily(family string) string {
	family = strings.ToLower(strings.TrimSpace(family))
	var b strings.Builder
	for _, r := range family {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	return b.String()
}
