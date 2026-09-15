package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"kobragames.local/launcher/internal/kobraerr"
)

// TestDrainRefusesNewRequests is the regression test for §10.6: once the drain
// flag is set, a request that starts after it must be refused with 503 and
// Connection: close so the shell knows the session is ending.
//
// The flag was set but nothing read it outside the watcher loop, and
// kobraerr.Draining existed with no caller — the behaviour was specified,
// implemented as a constructor, and never wired to a request.
func TestDrainRefusesNewRequests(t *testing.T) {
	h := newHarness(t)

	// Before the drain, a normal request is served.
	resp, err := h.ts.Client().Get(h.ts.URL + "/__kobra/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-drain status = %d, want 200", resp.StatusCode)
	}

	h.srv.draining.Store(true)
	t.Cleanup(func() { h.srv.draining.Store(false) })

	resp, err = h.ts.Client().Get(h.ts.URL + "/__kobra/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status during drain = %d, want 503", resp.StatusCode)
	}
	// The refusal still carries the §13.7 header set, because securityHeaders is
	// outermost.
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("the drain refusal lost the §13.7 headers")
	}

	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("the drain refusal is not a JSON envelope: %v", err)
	}
	if env["message"] != "The launcher is shutting down." {
		t.Errorf("message = %v, want the draining wording", env["message"])
	}
	if env["error"] != kobraerr.CodeReadOnly {
		t.Errorf("error code = %v, want %v (kobraerr.Draining's code)", env["error"], kobraerr.CodeReadOnly)
	}

	// Connection is hop-by-hop, so Go's client removes it from the response it
	// hands back. Read the wire to see what the server actually sent, which is
	// what the spec requires and what a shell sees.
	if !rawResponseHasConnectionClose(t, h) {
		t.Error("the drain refusal did not send Connection: close")
	}
}

// rawResponseHasConnectionClose speaks HTTP/1.1 over a raw socket, because the
// net/http client cannot observe a hop-by-hop header.
func rawResponseHasConnectionClose(t *testing.T, h *harness) bool {
	t.Helper()
	host := h.ts.Listener.Addr().String()
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET /__kobra/health HTTP/1.1\r\nHost: %s\r\n\r\n", host); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	head := string(raw)
	if i := strings.Index(head, "\r\n\r\n"); i >= 0 {
		head = head[:i]
	}
	if !strings.HasPrefix(head, "HTTP/1.1 503") {
		t.Fatalf("raw status line = %q, want 503", strings.SplitN(head, "\r\n", 2)[0])
	}
	return strings.Contains(strings.ToLower(head), "connection: close")
}
