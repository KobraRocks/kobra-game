package sidecar

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
)

// startFakeHealth serves GET /__kobra/health on a loopback port so the liveness
// check of §9.3 can be exercised against a real socket.
func startFakeHealth(t *testing.T, app, gameID, instance string) (uint16, func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__kobra/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(HealthResponse{
			App: app, GameID: gameID, Instance: instance,
		})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	return port, func() { _ = srv.Close() }
}
