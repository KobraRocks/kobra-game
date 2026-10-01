package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"kobragames.local/launcher/internal/config"
	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/paths"
	"kobragames.local/launcher/internal/storage"
)

// TestDiagnosticsReportsDevOnlyFromADevBuild pins the property the game's
// development sidebar rests on (Launcher spec §21.4, FR-LNCH-1).
//
// A game folder and its shell are byte-identical in a development run and in a
// packaged one, so a page cannot tell them apart by inspecting itself. The
// launcher already knows, because `kobra_dev` is a build tag; this asserts the
// payload reports it, and — the load-bearing half — that it is *omitted* when
// the binary is not a development build, so "no dev field" is the release
// answer and no request or flag can turn it on.
func TestDiagnosticsReportsDevOnlyFromADevBuild(t *testing.T) {
	dir := t.TempDir()
	log, err := diagnostics.Open(diagnostics.Options{
		LogDir: filepath.Join(dir, "logs"),
		Level:  diagnostics.LevelError,
	})
	if err != nil {
		t.Fatalf("diagnostics.Open: %v", err)
	}
	defer func() { _ = log.Close() }()

	eng, err := storage.New(storage.Options{
		DataDir:         dir,
		DataDirKind:     "game",
		KeepRevisions:   8,
		MaxRequestBytes: 1 << 20,
		GameID:          "com.kobra.devtest",
		Release:         "2026.09.1",
		EngineVersion:   "0.1.0",
		SaveVersion:     1,
		Logger:          log,
	})
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}

	root := paths.Root{
		GameFolder:  dir,
		GameDir:     dir,
		DataDir:     dir,
		LauncherDir: dir,
		SidecarDir:  dir,
		LogDir:      filepath.Join(dir, "logs"),
		DataDirKind: "game",
	}

	for _, dev := range []bool{false, true} {
		srv := New(Options{
			Root:    root,
			Cfg:     config.Config{},
			Storage: eng,
			Log:     log,
			Version: "test",
			Release: "2026.09.1",
			Dev:     dev,
			Ctx:     context.Background(),
		})
		raw, err := json.Marshal(srv.Diagnostics())
		if err != nil {
			t.Fatalf("marshal diagnostics: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("unmarshal diagnostics: %v", err)
		}
		value, present := decoded["dev"]
		if present != dev {
			t.Fatalf("dev=%v: payload carried dev=%v (%s)", dev, present, raw)
		}
		if dev && value != true {
			t.Fatalf("a development payload must report dev true: %s", raw)
		}
	}
}
