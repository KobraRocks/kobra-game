//go:build kobra_faultinject

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"kobragames.local/launcher/internal/faultinject"
)

// TestInjectedPanicGoesThroughTheRealChain is scenario 5 of §26.4 at the
// middleware level: with the panic armed, one request through the composed
// handler must produce the 500 io_error envelope, a crash dump, a drain request,
// and the Panicked() flag that main turns into exit code 4.
//
// The harness is built first on purpose — its setup issues requests, and an empty
// plan during setup is what lets the injected panic land on the request this test
// makes. ArmForTest exists for exactly that ordering.
func TestInjectedPanicGoesThroughTheRealChain(t *testing.T) {
	h := newHarness(t)
	faultinject.ArmForTest(faultinject.ServerHandler + "=panic")
	t.Cleanup(func() { faultinject.ArmForTest("") })

	resp, err := h.ts.Client().Get(h.ts.URL + "/index.html")
	if err != nil {
		t.Fatalf("the request failed before recovery could answer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("the panic response lost the §13.7 headers")
	}
	if !h.srv.Panicked() {
		t.Error("Panicked() = false after an injected panic; main would exit 0 instead of 4")
	}
	select {
	case reason := <-h.srv.shutdownCh:
		if reason != "panic" {
			t.Errorf("shutdown reason = %q, want panic", reason)
		}
	default:
		t.Error("the injected panic did not request a drain")
	}

	// The crash dump is what a support request needs.
	matches, err := filepath.Glob(filepath.Join(h.srv.root.LogDir, "crash-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("crash dumps written = %d, want 1", len(matches))
	} else if st, err := os.Stat(matches[0]); err != nil || st.Size() == 0 {
		t.Errorf("the crash dump is missing or empty: %v", err)
	}
}
