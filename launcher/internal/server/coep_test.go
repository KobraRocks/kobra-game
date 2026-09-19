package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"kobragames.local/launcher/internal/config"
)

// TestCOEPHeaderFollowsConfig is the regression test for FR-SRV-16: the header
// is present exactly when the publisher asked for cross-origin isolation, and
// COOP same-origin is always present because it is half of the isolation
// condition.
func TestCOEPHeaderFollowsConfig(t *testing.T) {
	cases := []struct {
		coep string
		want string
	}{
		{"", ""},
		{"require-corp", "require-corp"},
		{"credentialless", "credentialless"},
	}
	for _, tc := range cases {
		cfg := config.Default()
		cfg.Server.CrossOriginEmbedderPolicy = tc.coep
		s := &Server{cfg: cfg}

		rec := httptest.NewRecorder()
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		s.securityHeaders(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))

		if got := rec.Header().Get("Cross-Origin-Embedder-Policy"); got != tc.want {
			t.Errorf("COEP with config %q = %q, want %q", tc.coep, got, tc.want)
		}
		if got := rec.Header().Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
			t.Errorf("COOP = %q, want same-origin", got)
		}
		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want the wrapped handler's 204", rec.Code)
		}
	}
}
