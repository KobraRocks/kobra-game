//go:build kobra_faultinject

package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"kobragames.local/launcher/internal/faultinject"
	"kobragames.local/launcher/internal/kobraerr"
)

// TestRenamedCrossDeviceFallbackCompletesTheWrite is scenario 3 of §26.4: the
// rename is forced to fail with EXDEV, and the write must still complete through
// renameCrossDevice, with the degradation recorded rather than hidden.
//
// Before the injection hook this needed two real filesystems, so it was skipped
// on any machine whose temp directories shared one.
func TestRenamedCrossDeviceFallbackCompletesTheWrite(t *testing.T) {
	faultinject.ArmForTest(faultinject.StorageRename + "=exdev")
	t.Cleanup(func() { faultinject.ArmForTest("") })

	e, dir := newEngine(t)
	res, err := e.WriteSave(context.Background(), SaveWrite{
		Slot: "slot1", IfRevision: ptr(int64(0)), Payload: json.RawMessage(`{"hp":5}`),
	})
	if err != nil {
		t.Fatalf("WriteSave over the fallback: %v", err)
	}
	if res.Revision != 1 {
		t.Errorf("revision = %d, want 1", res.Revision)
	}
	if !e.AtomicityDegraded() {
		t.Error("the EXDEV fallback ran but AtomicityDegraded() is false, so diagnostics would not show it")
	}

	// The data must be readable through the normal path.
	payload, err := e.ReadSave(context.Background(), "slot1")
	if err != nil {
		t.Fatalf("ReadSave after the fallback: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got["hp"] != float64(5) {
		t.Errorf("payload = %v, want the written value", got)
	}
	// No temp file may survive a completed write.
	if found := strayTemps(t, filepath.Join(dir, "data", "saves")); len(found) != 0 {
		t.Errorf("the fallback left temp files behind: %v", found)
	}
}

// TestENOSPCOnWriteIsReportedAsInsufficientSpace is scenario 4 of §26.4: a disk
// that fills mid-write must map to the documented envelope, and the previous
// revision must survive it.
func TestENOSPCOnWriteIsReportedAsInsufficientSpace(t *testing.T) {
	e, dir := newEngine(t)
	ctx := context.Background()

	first, err := e.WriteSave(ctx, SaveWrite{
		Slot: "slot1", IfRevision: ptr(int64(0)), Payload: json.RawMessage(`{"hp":1}`),
	})
	if err != nil {
		t.Fatalf("the setup write failed: %v", err)
	}

	faultinject.ArmForTest(faultinject.StorageWrite + "=enospc")
	t.Cleanup(func() { faultinject.ArmForTest("") })

	_, err = e.WriteSave(ctx, SaveWrite{
		Slot: "slot1", IfRevision: ptr(first.Revision), Payload: json.RawMessage(`{"hp":2}`),
	})
	if err == nil {
		t.Fatal("a full disk did not fail the write")
	}
	ke := kobraerr.From(err)
	if ke.Code != kobraerr.CodeIO {
		t.Errorf("error code = %q, want %q", ke.Code, kobraerr.CodeIO)
	}
	if ke.Detail["reason"] != "insufficient_space" {
		t.Errorf("detail.reason = %v, want insufficient_space", ke.Detail["reason"])
	}
	if ke.HTTP != 500 {
		t.Errorf("HTTP status = %d, want the documented 500", ke.HTTP)
	}

	// The failed write must not have disturbed what was already there.
	payload, rerr := e.ReadSave(ctx, "slot1")
	if rerr != nil {
		t.Fatalf("the previous revision is no longer readable: %v", rerr)
	}
	if !strings.Contains(string(payload), `"hp":1`) {
		t.Errorf("the previous revision was overwritten by a failed write: %s", payload)
	}
	if found := strayTemps(t, filepath.Join(dir, "data", "saves")); len(found) != 0 {
		t.Errorf("a failed write left temp files behind: %v", found)
	}
}

// TestIsCrossDeviceRecognisesTheInjectedError keeps the injector honest: the
// synthetic error has to be the same shape the platform produces, or the
// fallback test above would exercise a branch production never takes.
func TestIsCrossDeviceRecognisesTheInjectedError(t *testing.T) {
	faultinject.ArmForTest(faultinject.StorageRename + "=exdev")
	t.Cleanup(func() { faultinject.ArmForTest("") })

	err := faultinject.Err(faultinject.StorageRename)
	if err == nil {
		t.Fatal("no error was injected")
	}
	if !isEXDEV(err) {
		t.Errorf("the injected error %v is not recognised as EXDEV", err)
	}
	if !errors.Is(err, syscall.EXDEV) {
		t.Errorf("errors.Is(%v, EXDEV) is false", err)
	}
}

// strayTemps lists the writeAtomic temp files left in dir.
func strayTemps(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range entries {
		if isWriteTempName(ent.Name()) {
			out = append(out, ent.Name())
		}
	}
	return out
}
