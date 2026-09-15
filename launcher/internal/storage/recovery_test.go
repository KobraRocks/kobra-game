package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestReadConfigRecoversFromBackupWhenLiveIsMissing is the regression test for
// the asymmetry with saves: writeAtomic rotates the live file to .bak before
// renaming the new one into place, so a failure between those steps leaves only
// the .bak. ReadConfig used to report an empty document, and MergeConfig then
// wrote a merge of nothing, silently dropping every setting the user had.
func TestReadConfigRecoversFromBackupWhenLiveIsMissing(t *testing.T) {
	e, _ := newEngine(t)
	ctx := context.Background()

	previous := []byte("{\"volume\":0.5,\"difficulty\":\"hard\"}\n")
	if err := os.MkdirAll(filepath.Dir(e.configPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.configPath()+".bak", previous, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := e.ReadConfig(ctx)
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if string(got) != string(previous) {
		t.Errorf("ReadConfig = %q, want the .bak revision %q", got, previous)
	}

	// The merge must build on the recovered document, not on an empty object.
	if _, err := e.MergeConfig(ctx, ConfigMerge{
		Merge: map[string]json.RawMessage{"volume": json.RawMessage(`0.9`)},
	}); err != nil {
		t.Fatalf("MergeConfig: %v", err)
	}

	after, err := e.ReadConfig(ctx)
	if err != nil {
		t.Fatalf("ReadConfig after merge: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("merged config is not JSON: %v (%s)", err, after)
	}
	if doc["volume"] != 0.9 {
		t.Errorf("merged volume = %v, want 0.9", doc["volume"])
	}
	if doc["difficulty"] != "hard" {
		t.Errorf("a key that existed only in the recovered revision was dropped: %v", doc)
	}
}

// TestReadSaveRecoversFromBackupWhenLiveIsMissing is the same failure window for
// a save: the live slot is gone and only .bak remains. Reporting NotFound would
// read as save loss to the player.
func TestReadSaveRecoversFromBackupWhenLiveIsMissing(t *testing.T) {
	e, _ := newEngine(t)
	ctx := context.Background()

	first, err := e.WriteSave(ctx, SaveWrite{
		Slot: "slot1", IfRevision: ptr(int64(0)), Payload: json.RawMessage(`{"hp":7}`),
	})
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := e.WriteSave(ctx, SaveWrite{
		Slot: "slot1", IfRevision: ptr(first.Revision), Payload: json.RawMessage(`{"hp":8}`),
	}); err != nil {
		t.Fatalf("second write: %v", err)
	}

	p, err := e.savePath("slot1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	got, err := e.ReadSave(ctx, "slot1")
	if err != nil {
		t.Fatalf("ReadSave with only .bak present: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("recovered payload is not JSON: %v (%s)", err, got)
	}
	if payload["hp"] != float64(7) {
		t.Errorf("recovered hp = %v, want the previous revision's 7", payload["hp"])
	}
}

// TestQuarantineStrayTempsPreservesTheInterruptedWrite covers what §26.4 asserts
// after a crash between fsync and rename: the temp file an interrupted writeAtomic
// left behind is moved to the trash area — never deleted — and the data it was
// replacing is untouched.
func TestQuarantineStrayTempsPreservesTheInterruptedWrite(t *testing.T) {
	e, dir := newEngine(t)
	ctx := context.Background()

	if _, err := e.WriteSave(ctx, SaveWrite{
		Slot: "slot1", IfRevision: ptr(int64(0)), Payload: json.RawMessage(`{"hp":1}`),
	}); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	// The exact name writeAtomic uses: ".<name>.tmp-<pid>-<seq>".
	savesDir := filepath.Join(dir, "data", "saves")
	stray := filepath.Join(savesDir, ".slot1.json.tmp-4242-7")
	if err := os.WriteFile(stray, []byte(`{"schema":"kobra.save/1"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := e.QuarantineStrayTemps(ctx)
	if err != nil {
		t.Fatalf("QuarantineStrayTemps: %v", err)
	}
	if n != 1 {
		t.Errorf("quarantined %d files, want 1", n)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("the stray temp file is still in saves/")
	}
	quarantined, err := filepath.Glob(filepath.Join(dir, "data", ".trash-*", "saves", ".*.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 1 {
		t.Errorf("the trash area holds %d files, want 1: %v", len(quarantined), quarantined)
	}

	// Nothing else was disturbed.
	if _, err := e.ReadSave(ctx, "slot1"); err != nil {
		t.Errorf("the interrupted write's predecessor is no longer readable: %v", err)
	}

	// A directory that merely looks odd is not touched: only the temp pattern is.
	if err := os.WriteFile(filepath.Join(savesDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err := e.QuarantineStrayTemps(ctx); err != nil || n != 0 {
		t.Errorf("a second pass quarantined %d files (err %v), want 0", n, err)
	}
	if _, err := os.Stat(filepath.Join(savesDir, "notes.txt")); err != nil {
		t.Error("an unrelated file in saves/ was quarantined")
	}
}

// TestCancelledContextWritesNothing pins the half of the cancellation contract
// that is observable: a context cancelled before the write starts is still an
// error, and nothing is created. The other half — a cancellation that arrives
// after the rename — intentionally reports success, because the data is already
// durable; that window needs the §26.4 fault-injection build to force, so this
// test asserts the pre-write behaviour only.
func TestCancelledContextWritesNothing(t *testing.T) {
	e, _ := newEngine(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := e.MergeConfig(cancelled, ConfigMerge{
		Merge: map[string]json.RawMessage{"volume": json.RawMessage(`0.5`)},
	}); err == nil {
		t.Error("MergeConfig with a cancelled context returned no error")
	}
	if _, err := os.Stat(e.configPath()); !os.IsNotExist(err) {
		t.Errorf("a cancelled write created the config file (stat err = %v)", err)
	}
}
