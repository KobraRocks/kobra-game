package config

import "testing"

// configWith builds a minimal valid config document with the given server
// fragment spliced in, so each case exercises Parse rather than a hand-built
// struct.
func configWith(serverJSON string) []byte {
	return []byte(`{
	  "schema": "kobra.launcher-config/1",
	  "game_id": "com.kobra.worldspiracy",
	  "game_name": "Worldspiracy",
	  "release": "2026.10.1",
	  "port": { "base": 18950, "span": 50 },
	  "server": {` + serverJSON + `}
	}`)
}

func TestEntryPathDefaultsToIndex(t *testing.T) {
	cfg, err := Parse(configWith(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Server.EntryPath != DefaultEntryPath {
		t.Fatalf("entry_path = %q, want %q", cfg.Server.EntryPath, DefaultEntryPath)
	}
}

// TestEntryPathAcceptsEveryServedDocument pins the accepted set to the documents
// the static handler actually serves. A value outside it would leave the key
// silently inert, so it must be rejected at load (FR-LNCH-9).
func TestEntryPathAcceptsEveryServedDocument(t *testing.T) {
	for _, path := range EntryDocuments() {
		cfg, err := Parse(configWith(`"entry_path": "` + path + `"`))
		if err != nil {
			t.Errorf("entry_path %q rejected: %v", path, err)
			continue
		}
		if cfg.Server.EntryPath != path {
			t.Errorf("entry_path = %q, want %q", cfg.Server.EntryPath, path)
		}
	}
}

func TestEntryPathRejectsUnservedDocument(t *testing.T) {
	for _, path := range []string{"/editor.html", "/shell.js", "/secrets", "editor", "/index.html/../../etc/passwd"} {
		if _, err := Parse(configWith(`"entry_path": "` + path + `"`)); err == nil {
			t.Errorf("entry_path %q accepted, want rejection", path)
		}
	}
}

func TestCOEPDefaultsToNoHeader(t *testing.T) {
	cfg, err := Parse(configWith(""))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Server.CrossOriginEmbedderPolicy != "" {
		t.Fatalf("COEP = %q, want empty by default", cfg.Server.CrossOriginEmbedderPolicy)
	}
	if cfg.Server.Isolated() {
		t.Fatal("a default config must not claim cross-origin isolation")
	}
}

func TestCOEPAcceptsOnlyIsolationValues(t *testing.T) {
	for _, v := range CrossOriginEmbedderPolicies {
		cfg, err := Parse(configWith(`"cross_origin_embedder_policy": "` + v + `"`))
		if err != nil {
			t.Errorf("COEP %q rejected: %v", v, err)
			continue
		}
		if !cfg.Server.Isolated() {
			t.Errorf("COEP %q must report Isolated()", v)
		}
	}
	for _, v := range []string{"require-corp ", "REQUIRE-CORP", "unsafe-none", "omit", "true"} {
		if _, err := Parse(configWith(`"cross_origin_embedder_policy": "` + v + `"`)); err == nil {
			t.Errorf("COEP %q accepted, want rejection", v)
		}
	}
}

// TestMimeTypesMergeOverDefaults pins the behaviour the packaging docs depend
// on: a config that declares one extension must not drop the built-in map,
// because .wasm must stay application/wasm or instantiateStreaming refuses the
// engine (FR-SRV-13).
func TestMimeTypesMergeOverDefaults(t *testing.T) {
	cfg, err := Parse(configWith(`"drain_timeout_seconds": 15`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.MimeTypes[".wasm"] != "application/wasm" {
		t.Fatalf(".wasm = %q, want application/wasm", cfg.MimeTypes[".wasm"])
	}

	declared, err := Parse([]byte(`{
	  "schema": "kobra.launcher-config/1",
	  "game_id": "com.kobra.worldspiracy",
	  "game_name": "Worldspiracy",
	  "release": "2026.10.1",
	  "port": { "base": 18950, "span": 50 },
	  "mime_types": { ".wgsl": "text/plain" }
	}`))
	if err != nil {
		t.Fatalf("parse with mime_types: %v", err)
	}
	if declared.MimeTypes[".wgsl"] != "text/plain" {
		t.Errorf(".wgsl = %q, want text/plain", declared.MimeTypes[".wgsl"])
	}
	if declared.MimeTypes[".wasm"] != "application/wasm" {
		t.Errorf(".wasm = %q after declaring .wgsl; mime_types must merge over the defaults, not replace them",
			declared.MimeTypes[".wasm"])
	}
	if declared.MimeTypes[".ogg"] != "audio/ogg" {
		t.Errorf(".ogg = %q after declaring .wgsl; defaults were dropped", declared.MimeTypes[".ogg"])
	}
}
