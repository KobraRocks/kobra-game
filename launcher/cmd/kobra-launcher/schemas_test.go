package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestSchemaCopiesMatchThePublishedSet keeps the launcher's vendored schema
// copies honest.
//
// architecture/schemas is the published set (FS Appendix B). The launcher keeps
// three kinds of copy: launcher/schemas/ is the vendored set a publisher reads,
// and two embedded copies exist because a copy under go:embed cannot reach
// outside its package: config/schema/launcher.config.schema.json and
// server/testschema/data-api.schema.json. Nothing compared them, so a schema
// change could silently reach only one of the five locations.
//
// The packaging module has the same test for its own copies; this is the
// launcher's half.
func TestSchemaCopiesMatchThePublishedSet(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..")
	archDir := filepath.Join(repoRoot, "architecture", "schemas")
	if _, err := os.Stat(archDir); err != nil {
		t.Skip("architecture/schemas is not present in this checkout")
	}

	copies := []string{
		filepath.Join("..", "..", "schemas"),
		filepath.Join("..", "..", "internal", "config", "schema"),
		filepath.Join("..", "..", "internal", "server", "testschema"),
	}

	// Every vendored copy must exist in the published set and be byte-identical.
	for _, dir := range copies {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			got, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			want, err := os.ReadFile(filepath.Join(archDir, entry.Name()))
			if err != nil {
				t.Errorf("%s is vendored but missing from architecture/schemas", entry.Name())
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from architecture/schemas/%s; re-copy it", entry.Name(), entry.Name())
			}
		}
	}

	// The published set must not contain a schema the launcher claims to ship,
	// or the reverse: a missing vendored copy is how the drift starts.
	archEntries, err := os.ReadDir(archDir)
	if err != nil {
		t.Fatal(err)
	}
	vendored := map[string]bool{}
	entries, err := os.ReadDir(copies[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		vendored[entry.Name()] = true
	}
	for _, entry := range archEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if !vendored[entry.Name()] {
			t.Errorf("architecture/schemas/%s is not vendored into launcher/schemas", entry.Name())
		}
	}
}
