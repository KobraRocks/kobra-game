package diagnostics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withDeadline runs fn and fails the test if it has not returned in d. It is
// how the rotation tests detect a regression of the self-deadlock fixed in
// rotateLocked: a blocked writer holds l.mu forever, so the test must not wait
// for the package-wide timeout to notice.
func withDeadline(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %v (deadlock?)", what, d)
	}
}

func openTestLogger(t *testing.T, maxBytes int64, generations int) *Logger {
	t.Helper()
	l, err := Open(Options{
		LogDir:      t.TempDir(),
		MaxBytes:    maxBytes,
		Generations: generations,
		Level:       LevelInfo,
		Version:     "test",
		Commit:      "test",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l
}

// TestRotateDoesNotDeadlock is the regression test for the rotation
// self-deadlock: rotateLocked used to call Event, which re-locked l.mu while
// the rotating goroutine already held it. Every line here is larger than
// maxBytes, so every write rotates.
func TestRotateDoesNotDeadlock(t *testing.T) {
	l := openTestLogger(t, 64, 2)

	withDeadline(t, 10*time.Second, "200 rotating writes", func() {
		for i := 0; i < 200; i++ {
			l.Info("tick", map[string]any{"i": i})
		}
	})

	// The rotate record must still be emitted, and the generations shifted.
	lines := strings.Join(l.Tail(0), "\n")
	if !strings.Contains(lines, `"evt":"log.rotate"`) {
		t.Errorf("the ring carries no log.rotate record after rotation; tail=%s", lines)
	}
	if !strings.Contains(lines, `"evt":"tick"`) {
		t.Errorf("the ring lost the caller's records; tail=%s", lines)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(l.path), "launcher.log.1")); err != nil {
		t.Errorf("rotation did not produce launcher.log.1: %v", err)
	}

	withDeadline(t, 5*time.Second, "Close", func() { _ = l.Close() })
}

// TestForceRotateDoesNotDeadlock covers the SIGHUP path, which reaches the same
// rotateLocked while holding l.mu.
func TestForceRotateDoesNotDeadlock(t *testing.T) {
	l := openTestLogger(t, 1<<20, 2)
	l.Info("before", nil)
	withDeadline(t, 10*time.Second, "ForceRotate", func() { l.ForceRotate() })
	withDeadline(t, 5*time.Second, "Close", func() { _ = l.Close() })
}

// TestLevelGate covers the enabledLocked refactor: the minimum level must still
// suppress lower-severity records.
func TestLevelGate(t *testing.T) {
	l := openTestLogger(t, 1<<20, 2)
	defer func() { _ = l.Close() }()

	l.Debug("suppressed", nil)
	l.Info("kept", nil)

	lines := strings.Join(l.Tail(0), "\n")
	if strings.Contains(lines, "suppressed") {
		t.Errorf("a debug record was emitted at info level: %s", lines)
	}
	if !strings.Contains(lines, `"evt":"kept"`) {
		t.Errorf("the info record is missing: %s", lines)
	}
}

// TestTailBounds pins the ring semantics after Tail was folded onto tailLocked.
func TestTailBounds(t *testing.T) {
	l := openTestLogger(t, 1<<20, 2)
	defer func() { _ = l.Close() }()

	for i := 0; i < 5; i++ {
		l.Info("e", map[string]any{"i": i})
	}
	if got := len(l.Tail(2)); got != 2 {
		t.Errorf("Tail(2) returned %d lines, want 2", got)
	}
	if got := len(l.Tail(0)); got != 5 {
		t.Errorf("Tail(0) returned %d lines, want all 5", got)
	}
	if got := len(l.Tail(99)); got != 5 {
		t.Errorf("Tail(99) returned %d lines, want 5", got)
	}
	// The most recent line is last.
	lines := l.Tail(1)
	if len(lines) != 1 || !strings.Contains(lines[0], `"i":4`) {
		t.Errorf("Tail(1) = %v, want the i=4 record", lines)
	}
}
