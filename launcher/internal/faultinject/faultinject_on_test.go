//go:build kobra_faultinject

package faultinject

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestPlanArmsEveryAction covers the plan syntax and each non-crashing action.
func TestPlanArmsEveryAction(t *testing.T) {
	resetForTest()
	t.Setenv("KOBRA_FAULT", "storage.rename=exdev,storage.write=enospc,server.handler=panic")

	if !Enabled() {
		t.Fatal("Enabled() is false with an armed plan")
	}

	if err := Err(StorageRename); !errors.Is(err, syscall.EXDEV) {
		t.Errorf("Err(storage.rename) = %v, want an EXDEV error", err)
	}
	if err := Err(StorageWrite); !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("Err(storage.write) = %v, want an ENOSPC error", err)
	}
	if err := Err(ServerHandler); err != nil {
		t.Errorf("Err(server.handler) = %v, want nil: it is armed to panic, not to fail", err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("Point(server.handler) did not panic")
			}
		}()
		Point(ServerHandler)
	}()
}

// TestPointsFireOnce pins the "a single injected fault must not become a storm"
// rule: a retry or a loop must not re-trigger the same point.
func TestPointsFireOnce(t *testing.T) {
	resetForTest()
	t.Setenv("KOBRA_FAULT", "storage.rename=exdev,server.handler=panic")

	if err := Err(StorageRename); err == nil {
		t.Fatal("the first Err did not fire")
	}
	if err := Err(StorageRename); err != nil {
		t.Errorf("the second Err fired again: %v", err)
	}
	if !Fired(StorageRename) {
		t.Error("Fired(storage.rename) is false after it fired")
	}

	for i := 1; i <= 2; i++ {
		panicked := func() (p bool) {
			defer func() { p = recover() != nil }()
			Point(ServerHandler)
			return false
		}()
		if i == 1 && !panicked {
			t.Error("the first Point(server.handler) did not panic")
		}
		if i == 2 && panicked {
			t.Error("the second Point(server.handler) panicked again")
		}
	}
}

// TestUnarmedPointsAreInert covers a plan that names other points.
func TestUnarmedPointsAreInert(t *testing.T) {
	resetForTest()
	t.Setenv("KOBRA_FAULT", "storage.rename=exdev")

	if err := Err(UpdatePromoteRename); err != nil {
		t.Errorf("Err(update.promote.rename) = %v, want nil: it is not in the plan", err)
	}
	Point(StorageWriteAfterRename) // must not exit
}

// TestMalformedPlanIsFatal pins the choice that a bad plan fails loudly: a test
// that silently ran without its fault would pass while proving nothing.
func TestMalformedPlanIsFatal(t *testing.T) {
	resetForTest()
	t.Setenv("KOBRA_FAULT", "no-equals-sign")
	defer func() {
		if recover() == nil {
			t.Fatal("a malformed plan did not panic")
		}
	}()
	Enabled()
}

// TestBadStatusCodeIsFatal covers the exit: specifier's parser.
func TestBadStatusCodeIsFatal(t *testing.T) {
	for _, spec := range []string{"exit:", "exit:abc", "exit:999"} {
		resetForTest()
		t.Setenv("KOBRA_FAULT", "storage.write.after_fsync="+spec)
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("plan %q did not panic", spec)
				}
			}()
			Enabled()
		}()
	}
}

// TestExitPointStopsTheProcess is the crash simulation itself, and it has to run
// in a real process to be meaningful: a crash point that does not actually stop
// the process would let a "crash" test continue and assert the wrong thing.
func TestExitPointStopsTheProcess(t *testing.T) {
	if os.Getenv("KOBRA_FAULT_HELPER") == "1" {
		// Child: arm the point and fire it.
		resetForTest()
		Point(StorageWriteAfterFsync) // armed by the parent's environment
		os.Exit(0)                    // reached only if the point did not fire
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestExitPointStopsTheProcess")
	cmd.Env = append(os.Environ(), "KOBRA_FAULT_HELPER=1", "KOBRA_FAULT=storage.write.after_fsync=exit:70")
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("the child returned %v, want an exit status", err)
	}
	if code := exit.ExitCode(); code != 70 {
		t.Errorf("the child exited %d, want the injected status 70", code)
	}
}
