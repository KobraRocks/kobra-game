//go:build linux

package browser

import (
	"strings"
	"testing"
)

// These tests exercise the Linux-only guidance path, so they carry the same
// build tag as guidance_linux.go.

func TestParseOSRelease(t *testing.T) {
	contents := "# comment\nNAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n"
	id, like := parseOSRelease(contents)
	if id != "ubuntu" || like != "debian" {
		t.Fatalf("parseOSRelease = (%q, %q), want (ubuntu, debian)", id, like)
	}
	id, like = parseOSRelease("ID=fedora\n")
	if id != "fedora" || like != "" {
		t.Fatalf("parseOSRelease = (%q, %q), want (fedora, \"\")", id, like)
	}
}

func TestInstallGuidanceNamesDetectedDistroCommand(t *testing.T) {
	// Skip only when the distro is unrecognised and there is no apt/dnf/... on
	// $PATH: then the generic package-manager wording is the correct output.
	cmd, ok := distroInstallCommand(detectDistroFamily())
	if !ok {
		t.Skip("distro family not recognised in this environment; generic wording applies")
	}
	g := InstallGuidance()
	if !strings.Contains(g, cmd) {
		t.Fatalf("InstallGuidance does not name the install command %q: %q", cmd, g)
	}
}
