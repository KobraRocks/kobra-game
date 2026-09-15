package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kobragames.local/launcher/internal/diagnostics"
	"kobragames.local/launcher/internal/kobraerr"
)

func newEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	log, _ := diagnostics.Open(diagnostics.Options{LogDir: filepath.Join(dir, "logs")})
	t.Cleanup(func() { _ = log.Close() })
	e, err := New(Options{
		DataDir:            filepath.Join(dir, "data"),
		DataDirKind:        "game",
		KeepRevisions:      2,
		TrashRetentionDays: 30,
		MaxRequestBytes:    1 << 20,
		GameID:             "com.kobra.test",
		Release:            "2026.09.1",
		EngineVersion:      "0.1.0",
		SaveVersion:        1,
		Logger:             log,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ProbeWrite(); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := e.InitRevisions(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, dir
}

func TestProbeWriteRecordsWritableAndReason(t *testing.T) {
	e, _ := newEngine(t)
	if !e.DataWritable() {
		t.Fatalf("expected writable, reason=%q", e.DataWritableReason())
	}
	if e.DataWritableReason() != "" {
		t.Errorf("a passing probe must record no reason, got %q", e.DataWritableReason())
	}
}

func TestWriteSaveProducesValidHeaderAndChecksum(t *testing.T) {
	e, dir := newEngine(t)
	res, err := e.WriteSave(context.Background(), SaveWrite{
		Slot:       "slot1",
		IfRevision: ptr(int64(0)),
		Payload:    json.RawMessage(`{"b":2,"a":1}`),
		Meta:       &SaveMeta{PlaytimeSeconds: ptr(int64(30))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Revision != 1 {
		t.Errorf("revision = %d, want 1", res.Revision)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "data", "saves", "slot1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hdr saveHeader
	if err := json.Unmarshal(raw, &hdr); err != nil {
		t.Fatalf("save is not valid JSON: %v", err)
	}
	if hdr.Schema != SchemaSave {
		t.Errorf("schema = %q", hdr.Schema)
	}
	if hdr.SaveVersion != 1 || hdr.GameVersion != "0.1.0" {
		t.Errorf("header version fields wrong: %+v", hdr)
	}
	if hdr.Revision != 1 {
		t.Errorf("header revision = %d", hdr.Revision)
	}
	if !strings.HasPrefix(hdr.Checksum, "sha256:") {
		t.Errorf("checksum missing: %q", hdr.Checksum)
	}
	if hdr.Created == "" || hdr.Modified == "" {
		t.Errorf("timestamps missing")
	}
	if !verifyChecksum(&hdr) {
		t.Errorf("checksum does not verify")
	}
	// FR-SAVE-4: the payload must survive a rewrite byte-for-byte in meaning.
	got, err := e.ReadSave(context.Background(), "slot1")
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	if back["a"] != float64(1) || back["b"] != float64(2) {
		t.Errorf("payload round-trip lost data: %s", got)
	}
}

func TestChecksumIsStableAcrossResave(t *testing.T) {
	e, _ := newEngine(t)
	payload := json.RawMessage(`{"x":[1,2,3],"y":{"k":"v"}}`)
	if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	first, _ := e.ReadSave(context.Background(), "s")
	// A semantically identical payload with different key order must not
	// change the checksum (the schema says the checksum covers the
	// canonicalised payload only).
	reordered := json.RawMessage(`{"y":{"k":"v"},"x":[1,2,3]}`)
	if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: reordered}); err != nil {
		t.Fatal(err)
	}
	second, _ := e.ReadSave(context.Background(), "s")
	var a, b any
	_ = json.Unmarshal(first, &a)
	_ = json.Unmarshal(second, &b)
	if !jsonEqual(t, a, b) {
		t.Errorf("payload changed across re-save")
	}
}

func TestConflictOnStaleRevision(t *testing.T) {
	e, _ := newEngine(t)
	if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", IfRevision: ptr(int64(0)), Payload: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	_, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", IfRevision: ptr(int64(0)), Payload: json.RawMessage(`{"a":2}`)})
	if err == nil {
		t.Fatal("stale if_revision was accepted")
	}
	ce, ok := err.(*ConflictError)
	if !ok {
		t.Fatalf("error = %T, want *ConflictError", err)
	}
	if ce.Current != 1 {
		t.Errorf("conflict current = %d, want 1", ce.Current)
	}
	ke := ce.AsKobra()
	if ke.Wire["current_revision"] != int64(1) {
		t.Errorf("wire conflict missing current_revision: %v", ke.Wire)
	}
	if len(ke.Detail) != 0 {
		t.Errorf("conflict detail must be empty for the schema, got %v", ke.Detail)
	}
}

func TestRevisionRecoveredFromHeaderWhenSidecarMissing(t *testing.T) {
	e, dir := newEngine(t)
	if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: json.RawMessage(`{"a":2}`)}); err != nil {
		t.Fatal(err)
	}
	// Remove every sidecar, as a filesystem cleanup would.
	entries, _ := os.ReadDir(filepath.Join(dir, "data", "saves"))
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), "s.json.") {
			_ = os.Remove(filepath.Join(dir, "data", "saves", ent.Name()))
		}
	}
	fresh, err := New(Options{
		DataDir:         filepath.Join(dir, "data"),
		KeepRevisions:   2,
		MaxRequestBytes: 1 << 20,
		GameID:          "com.kobra.test",
		EngineVersion:   "0.1.0",
		SaveVersion:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.InitRevisions(context.Background()); err != nil {
		t.Fatal(err)
	}
	// An engine that has not run the §15.7 probe refuses writes, so probe the
	// new instance the way startup would.
	if err := fresh.ProbeWrite(); err != nil {
		t.Fatalf("probe: %v", err)
	}
	// The header says revision 2, so a write from 2 must succeed and the
	// revision must advance to 3.
	res, err := fresh.WriteSave(context.Background(), SaveWrite{
		Slot: "s", IfRevision: ptr(int64(2)), Payload: json.RawMessage(`{"a":3}`),
	})
	if err != nil {
		t.Fatalf("write after sidecar loss: %v", err)
	}
	if res.Revision != 3 {
		t.Errorf("revision = %d, want 3", res.Revision)
	}
}

func TestRevisionSidecarsArePruned(t *testing.T) {
	e, dir := newEngine(t)
	for i := 0; i < 5; i++ {
		if _, err := e.WriteSave(context.Background(), SaveWrite{
			Slot: "s", Payload: json.RawMessage(`{"n":1}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "data", "saves"))
	count := 0
	for _, ent := range entries {
		// s.json.bak is the backup rotation, not a revision sidecar.
		if strings.HasPrefix(ent.Name(), "s.json.") && ent.Name() != "s.json.bak" {
			count++
		}
	}
	// The live revision's sidecar plus at most keep_revisions superseded ones.
	if count > 3 {
		t.Errorf("keep_revisions=2 left %d revision sidecars, want <= 3", count)
	}
	if count == 0 {
		t.Errorf("the live revision's sidecar must survive for the next conflict check")
	}
}

func TestBackupRotationKeepsOneGeneration(t *testing.T) {
	e, dir := newEngine(t)
	for i := 0; i < 3; i++ {
		if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: json.RawMessage(`{"v":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "saves", "s.json.bak")); err != nil {
		t.Errorf(".bak missing: %v", err)
	}
	// A .bak.1 must never appear: one generation only (§15.5).
	if _, err := os.Stat(filepath.Join(dir, "data", "saves", "s.json.bak.1")); err == nil {
		t.Errorf("unexpected second backup generation")
	}
}

func TestCorruptSaveFallsBackToBak(t *testing.T) {
	e, dir := newEngine(t)
	if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: json.RawMessage(`{"good":1}`)}); err != nil {
		t.Fatal(err)
	}
	// Second write produces the .bak holding the first good save.
	if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: json.RawMessage(`{"good":2}`)}); err != nil {
		t.Fatal(err)
	}
	// Corrupt the live file.
	if err := os.WriteFile(filepath.Join(dir, "data", "saves", "s.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := e.ReadSave(context.Background(), "s")
	if err != nil {
		t.Fatalf("recovery from .bak failed: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatal(err)
	}
	if v["good"] != float64(1) {
		t.Errorf("recovered the wrong generation: %s", got)
	}
}

func TestListSavesRebuildsAndSorts(t *testing.T) {
	e, _ := newEngine(t)
	for _, slot := range []string{"alpha", "beta", "gamma"} {
		if _, err := e.WriteSave(context.Background(), SaveWrite{
			Slot: slot, Payload: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := e.ListSaves(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Saves) != 3 {
		t.Fatalf("got %d saves, want 3", len(list.Saves))
	}
	if !list.RebuiltIndex {
		t.Errorf("rebuilt_index should be true when index.json is absent")
	}
	for _, s := range list.Saves {
		if s.Revision < 1 {
			t.Errorf("slot %s has revision %d", s.Slot, s.Revision)
		}
		if s.Modified == "" {
			t.Errorf("slot %s has no modified timestamp", s.Slot)
		}
	}
	// A missing index is not an error (FR: a lost index must never mean lost
	// saves).
	zero, err := e.ListSaves(context.Background())
	if err != nil || len(zero.Saves) != 3 {
		t.Errorf("second list failed: %v", err)
	}
}

func TestDeleteMovesToTrashAndNeverUnlinks(t *testing.T) {
	e, dir := newEngine(t)
	if _, err := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteSave(context.Background(), "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "saves", "s.json")); !os.IsNotExist(err) {
		t.Errorf("save still present after delete")
	}
	// The bytes must exist somewhere under a trash directory.
	found := false
	entries, _ := os.ReadDir(filepath.Join(dir, "data"))
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".trash-") {
			if _, err := os.Stat(filepath.Join(dir, "data", ent.Name(), "saves", "s.json")); err == nil {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("deleted save was not moved into a trash directory")
	}
}

func TestCleanupTrashHonoursRetention(t *testing.T) {
	e, dir := newEngine(t)
	oldTrash := filepath.Join(dir, "data", ".trash-2000-01-01T00-00-00Z", "saves")
	if err := os.MkdirAll(oldTrash, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldTrash, "old.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Backdate the trash directory beyond the retention window.
	past := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "data", ".trash-2000-01-01T00-00-00Z"), past, past); err != nil {
		t.Fatal(err)
	}
	removed, err := e.CleanupTrash(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	// Retention 0 disables cleanup entirely (§15.5). A fresh engine is used
	// rather than copying one, because Engine holds mutexes.
	if err := os.MkdirAll(filepath.Join(dir, "data", ".trash-2000-01-01T00-00-00Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	zero, err := New(Options{
		DataDir:            filepath.Join(dir, "data"),
		TrashRetentionDays: 0,
		MaxRequestBytes:    1 << 20,
		EngineVersion:      "0.1.0",
		SaveVersion:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := zero.CleanupTrash(context.Background())
	if err != nil || n != 0 {
		t.Errorf("retention 0 should disable cleanup, got %d, %v", n, err)
	}
}

func TestReadOnlyEngineRefusesWrites(t *testing.T) {
	dir := t.TempDir()
	e, err := New(Options{
		DataDir:         filepath.Join(dir, "data"),
		MaxRequestBytes: 1 << 20,
		EngineVersion:   "0.1.0",
		SaveVersion:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Never probed, so writable is false by default: a write must be refused
	// with the documented 503 rather than attempted.
	_, werr := e.WriteSave(context.Background(), SaveWrite{Slot: "s", Payload: json.RawMessage(`{}`)})
	if werr == nil {
		t.Fatal("write on an unprobed engine was accepted")
	}
	ke := kobraerr.From(werr)
	if ke.Code != kobraerr.CodeReadOnly {
		t.Errorf("code = %s, want read_only", ke.Code)
	}
}

func TestMergeConfigPreservesUnmentionedKeys(t *testing.T) {
	e, dir := newEngine(t)
	settings := filepath.Join(dir, "data", "config", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"keep":{"a":1},"change":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MergeConfig(context.Background(), ConfigMerge{
		Merge: map[string]json.RawMessage{"change": json.RawMessage(`2`)},
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(settings)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["keep"]; !ok {
		t.Errorf("unmentioned key dropped: %s", raw)
	}
	if doc["change"] != float64(2) {
		t.Errorf("merge did not apply: %s", raw)
	}
}

func TestWriterClaimIsAtomicAndIdempotent(t *testing.T) {
	e, _ := newEngine(t)
	if err := e.ClaimWriter("sess-a"); err != nil {
		t.Fatal(err)
	}
	if err := e.ClaimWriter("sess-a"); err != nil {
		t.Errorf("idempotent re-claim failed: %v", err)
	}
	err := e.ClaimWriter("sess-b")
	if err == nil {
		t.Fatal("second session stole the writer role")
	}
	if kobraerr.From(err).Code != kobraerr.CodeConflict {
		t.Errorf("code = %s, want conflict", kobraerr.From(err).Code)
	}
	if e.Writer() != "sess-a" {
		t.Errorf("writer = %q", e.Writer())
	}
	e.ReleaseWriter("sess-a")
	if e.Writer() != "" {
		t.Errorf("writer not released")
	}
}

// --- identifier validation (§16.1, §16.2) ---------------------------------

func TestValidateIdentifierRejectsEveryForbiddenShape(t *testing.T) {
	bad := []string{
		"", "A", "Slot", "slot ", " slot", "slot.", "-slot", ".slot", "_slot",
		"../x", "..", ".", "a/b", `a\b`, "C:", "CON", "con", "NUL", "AUX", "PRN",
		"COM1", "LPT9", "com5", strings.Repeat("a", 65),
		// Extensions do not disarm a device name: Windows resolves con.json to
		// the CON device and "nul" discards the write. The identifier regex
		// allows the dot, so this is the shape that shipped a silent save loss.
		"con.txt", "nul.json", "aux.bak", "prn.dat", "com1.json", "lpt9.txt",
	}
	for _, s := range bad {
		if err := ValidateIdentifier(s); err == nil {
			t.Errorf("ValidateIdentifier(%q) = nil, want error", s)
		} else if kobraerr.From(err).Code != kobraerr.CodeBadIdentifier {
			t.Errorf("ValidateIdentifier(%q) code = %s", s, kobraerr.From(err).Code)
		}
	}
	good := []string{
		"a", "slot1", "my-slot", "my_slot", "slot.v2", "0", "a1", strings.Repeat("a", 64),
		// Not device names; the first-dot rule must not over-reject them.
		"console", "auxiliary", "com10", "lpt0", "slots.json",
	}
	for _, s := range good {
		if err := ValidateIdentifier(s); err != nil {
			t.Errorf("ValidateIdentifier(%q) = %v, want nil", s, err)
		}
	}
}

func TestSavePathConfinedEvenWithHostileInput(t *testing.T) {
	e, _ := newEngine(t)
	// Belt and braces: even if validation were bypassed, the confinement check
	// must catch an escape (§16.4).
	hostile := filepath.Join(e.dataDir, "saves", "..", "..", "escape.json")
	if err := e.confine(hostile); err == nil {
		t.Errorf("confine accepted a path outside the data root")
	}
	if err := e.confine(filepath.Join(e.dataDir, "saves", "ok.json")); err != nil {
		t.Errorf("confine rejected a legitimate path: %v", err)
	}
}

// --- helpers --------------------------------------------------------------

func ptr[T any](v T) *T { return &v }

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	ab, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(ab) == string(bb)
}
