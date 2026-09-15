package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// securityHeaderNames is the §13.7 set every response must carry.
var securityHeaderNames = []string{
	"X-Content-Type-Options",
	"Referrer-Policy",
	"X-Frame-Options",
	"Cross-Origin-Opener-Policy",
	"Content-Security-Policy",
}

// TestSecurityHeadersOnGateRejections is the regression test for the gates
// answering before securityHeaders ran: a bad Host (421) and a bad Origin (403)
// used to be sent with no nosniff, no CSP and no X-Frame-Options, contradicting
// §13.7 and the comment on securityHeaders.
func TestSecurityHeadersOnGateRejections(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"bad host", func(r *http.Request) { r.Host = "evil.example" }, http.StatusMisdirectedRequest},
		{"bad origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, http.StatusForbidden},
		{"normal request", func(*http.Request) {}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.ts.URL+"/index.html", nil)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(req)
			resp, err := h.ts.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			for _, name := range securityHeaderNames {
				if resp.Header.Get(name) == "" {
					t.Errorf("%s is missing on a %d response", name, resp.StatusCode)
				}
			}
		})
	}
}

// TestRecoveredPanicIsReportedForExit4 covers FR-SRV-24: the panic is answered
// with 500 io_error, requests a drain, and is recorded so main can exit 4
// instead of reporting a normal shutdown.
func TestRecoveredPanicIsReportedForExit4(t *testing.T) {
	h := newHarness(t)

	if h.srv.Panicked() {
		t.Fatal("Panicked() is true before any panic")
	}

	rec := httptest.NewRecorder()
	h.srv.recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("review boom")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !h.srv.Panicked() {
		t.Error("Panicked() = false after a recovered panic; main would report exit 0")
	}
	select {
	case reason := <-h.srv.shutdownCh:
		if reason != "panic" {
			t.Errorf("shutdown reason = %q, want %q", reason, "panic")
		}
	default:
		t.Error("a recovered panic did not request a drain")
	}

	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("the panic response is not a JSON envelope: %v (%s)", err, rec.Body.String())
	}
	if env["error"] != "io_error" {
		t.Errorf("error code = %v, want io_error", env["error"])
	}
}
