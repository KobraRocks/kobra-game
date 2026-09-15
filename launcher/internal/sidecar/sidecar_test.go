package sidecar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPortStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := &State{
		GameID: "com.kobra.test",
		Port:   8771,
		Origin: Origin(8771),
	}
	if err := st.Save(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, PortFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("port.json is not valid JSON: %v", err)
	}
	if doc["schema"] != SchemaPortState {
		t.Errorf("schema = %v", doc["schema"])
	}
	if doc["port"] != float64(8771) {
		t.Errorf("port = %v", doc["port"])
	}
	if doc["origin"] != "http://127.0.0.1:8771" {
		t.Errorf("origin = %v", doc["origin"])
	}

	back, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if back == nil || back.Port != 8771 || back.GameID != "com.kobra.test" {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if back.Updated.IsZero() {
		t.Error("updated timestamp not set")
	}
}

// TestLoadMissingPortStateIsNotAnError covers §6.2: the deterministic default is
// used, not a failure.
func TestLoadMissingPortStateIsNotAnError(t *testing.T) {
	st, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load on an empty directory returned an error: %v", err)
	}
	if st != nil {
		t.Errorf("expected nil state, got %+v", st)
	}
}

func TestLoadRejectsCorruptOrOutOfRangeState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, PortFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("corrupt port.json was accepted")
	}

	if err := os.WriteFile(filepath.Join(dir, PortFileName),
		[]byte(`{"schema":"kobra.port-state/1","port":80}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a privileged port in port.json was accepted")
	}

	if err := os.WriteFile(filepath.Join(dir, PortFileName),
		[]byte(`{"schema":"kobra.other/9","port":8771}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("an unexpected schema was accepted")
	}
}

func TestRememberOriginCapsHistoryAndDedupes(t *testing.T) {
	var st State
	for i := 0; i < 8; i++ {
		st.RememberOrigin(Origin(uint16(8700 + i)))
	}
	if len(st.OriginHistory) != OriginHistoryCap {
		t.Fatalf("history length = %d, want %d", len(st.OriginHistory), OriginHistoryCap)
	}
	// Oldest-first eviction: the last five origins survive.
	if st.OriginHistory[0].Origin != Origin(8703) {
		t.Errorf("oldest entry = %q, want %q", st.OriginHistory[0].Origin, Origin(8703))
	}
	if st.OriginHistory[len(st.OriginHistory)-1].Origin != Origin(8707) {
		t.Errorf("newest entry = %q", st.OriginHistory[len(st.OriginHistory)-1].Origin)
	}
	// Re-remembering an existing origin moves it to the end rather than
	// duplicating it, so the support path of FS §6.6 keeps working.
	st.RememberOrigin(Origin(8705))
	if len(st.OriginHistory) != OriginHistoryCap {
		t.Fatalf("re-remember changed the length to %d", len(st.OriginHistory))
	}
	if st.OriginHistory[len(st.OriginHistory)-1].Origin != Origin(8705) {
		t.Errorf("re-remembered origin is not newest: %+v", st.OriginHistory)
	}
	seen := map[string]int{}
	for _, e := range st.OriginHistory {
		seen[e.Origin]++
	}
	if seen[Origin(8705)] != 1 {
		t.Errorf("origin duplicated in history: %+v", st.OriginHistory)
	}
}

func TestSaveIsAtomicAndKeepsBackup(t *testing.T) {
	dir := t.TempDir()
	first := &State{GameID: "com.kobra.test", Port: 8771, Origin: Origin(8771)}
	if err := first.Save(dir); err != nil {
		t.Fatal(err)
	}
	second := &State{GameID: "com.kobra.test", Port: 8772, Origin: Origin(8772)}
	if err := second.Save(dir); err != nil {
		t.Fatal(err)
	}
	// The previous generation is retained as .bak, and no temp file is left
	// behind (§15.3).
	if _, err := os.Stat(filepath.Join(dir, PortFileName+".bak")); err != nil {
		t.Errorf("no .bak after the second save: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		if len(ent.Name()) > 0 && ent.Name()[0] == '.' && filepath.Ext(ent.Name()) != ".bak" {
			t.Errorf("temp file left behind: %s", ent.Name())
		}
	}
	back, _ := Load(dir)
	if back.Port != 8772 {
		t.Errorf("port = %d, want 8772", back.Port)
	}
}

// --- instance lock (§9) ---------------------------------------------------

func TestAcquireAndReleaseLock(t *testing.T) {
	dir := t.TempDir()
	l, err := AcquireLock(dir, "com.kobra.test", "1.0.0", 8771, "inst-1")
	if err != nil {
		t.Fatal(err)
	}
	if l.Port() != 8771 {
		t.Errorf("port = %d", l.Port())
	}
	if l.Reclaimed() {
		t.Error("a fresh lock must not report reclamation")
	}
	info, state, err := ReadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if state == LockAbsent {
		t.Fatal("lock file was not created")
	}
	if info.GameID != "com.kobra.test" || info.Instance != "inst-1" {
		t.Errorf("lock identity wrong: %+v", info)
	}
	if info.PID != os.Getpid() {
		t.Errorf("pid = %d", info.PID)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, LockFileName)); !os.IsNotExist(err) {
		t.Error("lock file survived Release")
	}
	// Release is idempotent.
	if err := l.Release(); err != nil {
		t.Errorf("second Release: %v", err)
	}
}

// TestStaleLockIsReclaimed covers the §9.3/§9.6 path: a lock left by a dead
// process is reclaimed rather than blocking startup.
func TestStaleLockIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	stale := LockInfo{
		Schema:          SchemaInstanceLock,
		PID:             999999, // no such process
		StartTime:       time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		LauncherVersion: "1.0.0",
		GameID:          "com.kobra.test",
		Port:            8771,
	}
	raw, _ := json.Marshal(stale)
	if err := os.WriteFile(filepath.Join(dir, LockFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(dir, "com.kobra.test", "1.0.0", 8772, "inst-2")
	if err != nil {
		t.Fatalf("AcquireLock did not reclaim a stale lock: %v", err)
	}
	if !l.Reclaimed() {
		t.Error("reclamation not reported")
	}
	if l.Port() != 8772 {
		t.Errorf("port = %d, want the new 8772", l.Port())
	}
	_ = l.Release()
}

// TestUnparseableLockIsTreatedAsStale covers §9.2's "parse fails -> reclaim".
func TestUnparseableLockIsTreatedAsStale(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LockFileName), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(dir, "com.kobra.test", "1.0.0", 8773, "inst-3")
	if err != nil {
		t.Fatalf("AcquireLock did not reclaim an unparseable lock: %v", err)
	}
	if !l.Reclaimed() {
		t.Error("reclamation not reported")
	}
	_ = l.Release()
}

// TestLiveLockBlocksSecondInstance is the FR-SRV-25 election property. A live
// lock is simulated with a real health endpoint so §9.3's primary signal is
// exercised rather than only the PID backstop.
func TestLiveLockBlocksSecondInstance(t *testing.T) {
	dir := t.TempDir()
	port, stop := startFakeHealth(t, "kobra-launcher", "com.kobra.test", "inst-live")
	defer stop()

	live := LockInfo{
		Schema:          SchemaInstanceLock,
		PID:             os.Getpid(),
		StartTime:       time.Now().UTC().Format(time.RFC3339),
		LauncherVersion: "1.0.0",
		GameID:          "com.kobra.test",
		Port:            port,
		Instance:        "inst-live",
	}
	raw, _ := json.Marshal(live)
	if err := os.WriteFile(filepath.Join(dir, LockFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	ok, reason := CheckLiveness(dir, &live, 500*time.Millisecond)
	if !ok {
		t.Fatalf("a live instance was reported dead: %s", reason)
	}
	if _, err := AcquireLock(dir, "com.kobra.test", "1.0.0", 9999, "inst-new"); err == nil {
		t.Fatal("a second instance acquired a live lock")
	} else if _, ok := err.(*LockHeldError); !ok {
		t.Errorf("error = %T, want *LockHeldError", err)
	}
}

// TestLivenessDetectsWrongGameOnPort is the §9.3 backstop for a port reused by
// another process.
func TestLivenessDetectsWrongGameOnPort(t *testing.T) {
	dir := t.TempDir()
	port, stop := startFakeHealth(t, "kobra-launcher", "com.other.game", "inst-other")
	defer stop()
	info := LockInfo{
		Schema: SchemaInstanceLock, PID: os.Getpid(),
		StartTime: time.Now().UTC().Format(time.RFC3339),
		GameID:    "com.kobra.test", Port: port, Instance: "inst-wanted",
	}
	raw, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(dir, LockFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := CheckLiveness(dir, &info, 500*time.Millisecond); ok {
		t.Error("a different game on the recorded port was treated as live")
	}
}

func TestSetPortUpdatesHeldLock(t *testing.T) {
	dir := t.TempDir()
	l, err := AcquireLock(dir, "com.kobra.test", "1.0.0", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if err := l.SetPort(8771, "inst-x"); err != nil {
		t.Fatal(err)
	}
	info, _, err := ReadLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Port != 8771 || info.Instance != "inst-x" {
		t.Errorf("lock not updated in place: %+v", info)
	}
}
