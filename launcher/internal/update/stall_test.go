package update

import (
	"testing"
	"time"
)

// TestStallGuardDefaultsToStallTimeout is the regression test for the unarmed
// stall watchdog: production passes the zero value (ApplyOptions.StallTimeout is
// a test override), and zero must mean dlStallTimeout rather than "disabled".
func TestStallGuardDefaultsToStallTimeout(t *testing.T) {
	g := newStallGuard(0, func() {})
	if g.timeout != dlStallTimeout {
		t.Errorf("newStallGuard(0).timeout = %v, want the default %v", g.timeout, dlStallTimeout)
	}
	if n := newStallGuard(-time.Second, func() {}); n.timeout != dlStallTimeout {
		t.Errorf("newStallGuard(negative).timeout = %v, want the default %v", n.timeout, dlStallTimeout)
	}
	if e := newStallGuard(250*time.Millisecond, func() {}); e.timeout != 250*time.Millisecond {
		t.Errorf("an explicit timeout was overridden: %v", e.timeout)
	}
}

// TestStallGuardFires proves the guard actually cancels after the idle window,
// using a short explicit deadline so the test stays fast.
func TestStallGuardFires(t *testing.T) {
	fired := make(chan struct{})
	g := newStallGuard(20*time.Millisecond, func() { close(fired) })
	g.start()
	defer g.stop()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the stall guard never fired")
	}
}

// TestStallGuardTouchDefersFire proves activity resets the deadline, which is
// the property that lets a slow-but-progressing transfer survive.
func TestStallGuardTouchDefersFire(t *testing.T) {
	fired := make(chan struct{})
	g := newStallGuard(120*time.Millisecond, func() { close(fired) })
	g.start()
	defer g.stop()

	for i := 0; i < 5; i++ {
		time.Sleep(40 * time.Millisecond)
		g.touch()
	}
	select {
	case <-fired:
		t.Fatal("the guard fired despite continuous activity")
	default:
	}
}
