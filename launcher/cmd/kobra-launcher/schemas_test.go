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
// architecture/schemas is the published set (FS Appendix B) and the only place a
// schema is edited. The launcher keeps three directory copies — launcher/schemas/
// is the vendored set a publisher reads, and two embed directories exist because
// a copy under //go:embed cannot reach outside its package — plus one loose file,
// launcher/port-deny-list.json. The packaging module has the same test for its
// own copies.
//
// `make sync-schemas` regenerates every copy from the published set. This test is
// what fails when someone forgets to run it: a script nobody runs is not a guard.
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
				t.Errorf("%s is vendored but missing from architecture/schemas; run `make sync-schemas` at the repository root to remove it", entry.Name())
				continue
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from architecture/schemas/%s; run `make sync-schemas` at the repository root", entry.Name(), entry.Name())
			}
		}
	}

	// One copy does not live in a schema directory. The port deny list is read by
	// port_test.go and copied into a shipped game folder by `make package`,
	// `make run-dev`, .e2e/run.sh and test/faultinject.sh. A drifted copy is still
	// a valid document, so byte equality to the published set is the only thing
	// standing between a stale list and the players who receive it.
	for _, name := range []string{"port-deny-list.json"} {
		path := filepath.Join("..", "..", name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		want, err := os.ReadFile(filepath.Join(archDir, name))
		if err != nil {
			t.Errorf("launcher/%s is shipped but missing from architecture/schemas", name)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("launcher/%s differs from architecture/schemas/%s; run `make sync-schemas` at the repository root", name, name)
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
			t.Errorf("architecture/schemas/%s is not vendored into launcher/schemas; run `make sync-schemas` at the repository root", entry.Name())
		}
	}
}
