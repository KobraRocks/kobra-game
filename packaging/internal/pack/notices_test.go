package pack

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddedNoticesMatchRepository keeps LICENSES/THIRD_PARTY_NOTICES.md — the
// copy that ships inside every package — honest, the same way
// TestEmbeddedSchemasMatchArchitecture guards the vendored schemas.
//
// The file is a hand copy because kobra-pack runs from a publisher's machine and
// cannot read the source tree, so drift is possible and has legal consequences:
// a dependency bump that updates the repository's notices but not this copy would
// ship packages with stale attribution.
func TestEmbeddedNoticesMatchRepository(t *testing.T) {
	repo := filepath.Join("..", "..", "..", "THIRD_PARTY_NOTICES.md")
	onDisk, err := os.ReadFile(repo)
	if err != nil {
		t.Skip("THIRD_PARTY_NOTICES.md is not present in this checkout")
	}
	if !bytes.Equal(thirdPartyNotices, onDisk) {
		t.Errorf("the embedded third-party notices differ from %s; re-copy the file", repo)
	}
	if len(thirdPartyNotices) == 0 {
		t.Error("the embedded third-party notices are empty")
	}
}

// TestPackagesCarryTheNotices is the end-to-end half: the notices must actually
// be written into a built package, not merely embedded in the tool. A package
// ships launcher/launcher, so the licences of the code linked into it require
// their notices to travel with it.
func TestPackagesCarryTheNotices(t *testing.T) {
	out := buildFixtureDist(t)
	zipPath := filepath.Join(out, "fixture-2026.09.1-linux-x64.zip")

	notices := zipEntry(t, zipPath, "Fixture/LICENSES/THIRD_PARTY_NOTICES.md")
	if !bytes.Equal(notices, thirdPartyNotices) {
		t.Error("the packaged notices differ from the embedded copy")
	}
	for _, want := range []string{"klauspost/compress", "jsonschema", "golang.org/x/sys", "BurntSushi/toml", "Apache-2.0", "BSD-3-Clause"} {
		if !bytes.Contains(notices, []byte(want)) {
			t.Errorf("the packaged notices do not mention %q", want)
		}
	}

	readme := zipEntry(t, zipPath, "Fixture/LICENSES/README.txt")
	if !bytes.Contains(readme, []byte("THIRD_PARTY_NOTICES.md")) {
		t.Error("LICENSES/README.txt does not point at the notices file")
	}
	if !bytes.Contains(readme, []byte("MIT")) {
		t.Error("LICENSES/README.txt does not name the launcher's own licence")
	}
}
