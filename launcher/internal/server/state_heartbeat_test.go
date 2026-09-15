package server

import "testing"

// TestStatePublishesTheHeartbeatInterval is the regression test for the inert
// server.heartbeat_interval_seconds knob: the cadence belongs to the shell
// (FR-SRV-18), so the launcher must publish it through GET /api/state or the
// configured value can never reach the only component that uses it.
func TestStatePublishesTheHeartbeatInterval(t *testing.T) {
	h := newHarness(t)

	st, err := h.srv.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	// The harness config omits the key, so config.Parse's default applies.
	if st.HeartbeatIntervalSeconds != 5 {
		t.Errorf("heartbeat_interval_seconds = %d, want the documented default 5", st.HeartbeatIntervalSeconds)
	}

	// A publisher's value must be what is published.
	h.srv.cfg.Server.HeartbeatIntervalSeconds = 12
	st, err = h.srv.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.HeartbeatIntervalSeconds != 12 {
		t.Errorf("heartbeat_interval_seconds = %d, want the configured 12", st.HeartbeatIntervalSeconds)
	}
}
