//go:build kobra_faultinject

package faultinject

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
)

// Action is what an armed point does when it fires.
type Action int

const (
	// ActionNone means the point is not armed.
	ActionNone Action = iota
	// ActionExit stops the process, simulating a crash at that instant.
	ActionExit
	// ActionPanic panics on the calling goroutine.
	ActionPanic
	// ActionEXDEV returns a synthetic cross-device error from Err.
	ActionEXDEV
	// ActionENOSPC returns a synthetic out-of-space error from Err.
	ActionENOSPC
)

// armed is one point's configured action.
type armed struct {
	action Action
	code   int
}

var (
	mu       sync.Mutex
	plan     map[string]armed
	loaded   bool
	firedSet = map[string]bool{}
)

// load reads KOBRA_FAULT once per process. A malformed plan is a programming
// error in a test, so it is fatal rather than ignored: silently running without
// the fault a test asked for would produce a passing test that proves nothing.
func load() {
	if loaded {
		return
	}
	loaded = true
	plan = map[string]armed{}
	raw := strings.TrimSpace(os.Getenv("KOBRA_FAULT"))
	if raw == "" {
		return
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, spec, ok := strings.Cut(item, "=")
		if !ok {
			panic(fmt.Sprintf("faultinject: %q is not point=action", item))
		}
		name, spec = strings.TrimSpace(name), strings.TrimSpace(spec)
		a := armed{}
		switch {
		case spec == "panic":
			a.action = ActionPanic
		case spec == "exdev":
			a.action = ActionEXDEV
		case spec == "enospc":
			a.action = ActionENOSPC
		case strings.HasPrefix(spec, "exit:"):
			code, err := parseCode(strings.TrimPrefix(spec, "exit:"))
			if err != nil {
				panic(fmt.Sprintf("faultinject: %q: %v", item, err))
			}
			a.action = ActionExit
			a.code = code
		default:
			panic(fmt.Sprintf("faultinject: unknown action %q", spec))
		}
		plan[name] = a
	}
}

func parseCode(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("exit needs a status code")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("%q is not a status code", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	if n < 0 || n > 255 {
		return 0, fmt.Errorf("status %d is outside 0..255", n)
	}
	return n, nil
}

// take returns the action for name if it is armed, has not fired yet, and is one
// of the actions the caller can perform. It marks the point fired only in that
// case: an Err probe must not consume a Point's crash, and vice versa, because
// the two are separate mechanisms that happen to share a name space.
func take(name string, want ...Action) armed {
	load()
	mu.Lock()
	defer mu.Unlock()
	a, ok := plan[name]
	if !ok || firedSet[name] {
		return armed{}
	}
	for _, w := range want {
		if a.action == w {
			firedSet[name] = true
			return a
		}
	}
	return armed{}
}

// Point performs the configured action for name, if it is armed and has not
// fired yet.
func Point(name string) {
	switch a := take(name, ActionExit, ActionPanic); a.action {
	case ActionExit:
		// A raw exit, deliberately: the point is simulating a process that died
		// between two syscalls, with no chance to flush or unwind.
		os.Exit(a.code)
	case ActionPanic:
		panic("faultinject: " + name)
	}
}

// Err returns the platform error an armed point manufactures, or nil.
func Err(name string) error {
	switch a := take(name, ActionEXDEV, ActionENOSPC); a.action {
	case ActionEXDEV:
		return &os.LinkError{Op: "rename", Old: "faultinject", New: "faultinject", Err: syscall.EXDEV}
	case ActionENOSPC:
		return &os.PathError{Op: "write", Path: "faultinject", Err: syscall.ENOSPC}
	default:
		return nil
	}
}

// Enabled reports whether any point is armed.
func Enabled() bool {
	load()
	mu.Lock()
	defer mu.Unlock()
	return len(plan) > 0
}

// Fired reports whether a point has already fired, for assertions.
func Fired(name string) bool {
	load()
	mu.Lock()
	defer mu.Unlock()
	return firedSet[name]
}

// resetForTest clears the memoised plan and the fired set so a test can install
// another plan. Only compiled with the tag, and only used by this package's
// tests: one process, one plan is the production contract.
func resetForTest() {
	mu.Lock()
	defer mu.Unlock()
	loaded = false
	plan = nil
	firedSet = map[string]bool{}
}

// ArmForTest installs a plan after the process has already loaded one, which a
// test needs when the code under test has already served a request (and so
// memoised an empty plan). An empty plan disarms everything.
//
// It exists only in a kobra_faultinject build, so a release binary has no way to
// reach it.
func ArmForTest(plan string) {
	resetForTest()
	if strings.TrimSpace(plan) == "" {
		_ = os.Unsetenv("KOBRA_FAULT")
	} else {
		_ = os.Setenv("KOBRA_FAULT", plan)
	}
	load()
}
