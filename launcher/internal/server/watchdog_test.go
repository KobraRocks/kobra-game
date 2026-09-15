package server

import (
	"testing"
	"time"
)

// TestWatchdogFiresOnceAndNotices is the regression test for §20.5: the
// watchdog used to only write a log event, while the spec requires it to be
// surfaced (E22) with the log location. It is driven with synthetic times so the
// test does not wait twenty seconds.
func TestWatchdogFiresOnceAndNotices(t *testing.T) {
	h := newHarness(t)

	notices := 0
	var gotDir string
	h.srv.onWatchdogTimeout = func(dir string) {
		notices++
		gotDir = dir
	}

	started := time.Now()

	// Inside the window: silent.
	if h.srv.checkWatchdog(started, started.Add(watchdogTimeout-time.Second)) {
		t.Error("the watchdog fired before its window expired")
	}
	if notices != 0 {
		t.Errorf("the watchdog surfaced %d notices before its window expired", notices)
	}

	// Past the window: fires once, and surfaces exactly one notice.
	if !h.srv.checkWatchdog(started, started.Add(watchdogTimeout+time.Second)) {
		t.Error("the watchdog did not fire after its window expired")
	}
	if notices != 1 {
		t.Errorf("notices = %d, want 1", notices)
	}
	if gotDir != h.srv.root.LogDir {
		t.Errorf("the notice carried log dir %q, want %q", gotDir, h.srv.root.LogDir)
	}

	// It never fires twice, however long the page stays silent.
	if h.srv.checkWatchdog(started, started.Add(10*watchdogTimeout)) {
		t.Error("the watchdog fired a second time")
	}
	if notices != 1 {
		t.Errorf("notices = %d after a second tick, want 1", notices)
	}
}

// TestWatchdogCancelledByAHeartbeat covers the other half: once the shell has
// connected, a later silent period is the idle timeout's business, not the
// watchdog's.
func TestWatchdogCancelledByAHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.srv.Heartbeat()

	notified := false
	h.srv.onWatchdogTimeout = func(string) { notified = true }

	started := time.Now()
	if h.srv.checkWatchdog(started, started.Add(10*watchdogTimeout)) {
		t.Error("the watchdog fired after a heartbeat had already arrived")
	}
	if notified {
		t.Error("the watchdog surfaced a notice after a heartbeat had already arrived")
	}
}
