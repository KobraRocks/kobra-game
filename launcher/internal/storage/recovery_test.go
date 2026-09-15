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
