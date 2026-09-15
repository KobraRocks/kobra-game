package port

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"kobragames.local/launcher/internal/diagnostics"
)

// --- helpers ---------------------------------------------------------------

// testLogger returns a debug-level logger whose in-memory ring the test can
// read back as structured events.
func testLogger(t *testing.T) *diagnostics.Logger {
	t.Helper()
	l, err := diagnostics.Open(diagnostics.Options{LogDir: t.TempDir(), Level: diagnostics.LevelDebug})
	if err != nil {
		t.Fatalf("diagnostics.Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func logEvents(t *testing.T, l *diagnostics.Logger) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range l.Tail(0) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func eventNames(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		s, _ := e["evt"].(string)
		out = append(out, s)
	}
	return out
}

func eventsNamed(events []map[string]any, name string) []map[string]any {
	var out []map[string]any
	for _, e := range events {
		if e["evt"] == name {
			out = append(out, e)
		}
	}
	return out
}

func requireEvent(t *testing.T, events []map[string]any, name string) map[string]any {
	t.Helper()
	got := eventsNamed(events, name)
	if len(got) == 0 {
		t.Fatalf("event %q was not emitted; the log holds %v", name, eventNames(events))
	}
	return got[0]
}

func numField(t *testing.T, ev map[string]any, key string) float64 {
	t.Helper()
	v, ok := ev[key].(float64)
	if !ok {
		t.Fatalf("event %v field %q = %v (%T), want a number", ev["evt"], key, ev[key], ev[key])
	}
	return v
}

// --- port.probe / port.bind.failed / port.scan / port.change ---------------

// TestProbeLoggedEmitsPortProbe wires the one-port probe used by the
// --check-port fast path.
func TestProbeLoggedEmitsPortProbe(t *testing.T) {
	l := testLogger(t)
	p := freePort(t)

	res, err := ProbeLogged(l, p, 50*time.Millisecond, testGameID)
	if err != nil {
		t.Fatalf("ProbeLogged(%d): %v", p, err)
	}
	if res != ProbeFree {
		t.Fatalf("ProbeLogged(%d) = %v, want ProbeFree", p, res)
	}

	ev := requireEvent(t, logEvents(t, l), EventProbe)
	if port := numField(t, ev, "port"); port != float64(p) {
		t.Errorf("port.probe port = %v, want %d", port, p)
	}
	if got := ev["result"]; got != "free" {
		t.Errorf("port.probe result = %v, want free", got)
	}
}

// TestScanLoggedEmitsPortScan wires the direct scan call site (§8.3).
func TestScanLoggedEmitsPortScan(t *testing.T) {
	l := testLogger(t)
	p := freePort(t)

	got, err := ScanLogged(l, p, 1, emptyDenyList(t), nil, 50*time.Millisecond, testGameID)
	if err != nil {
		t.Fatalf("ScanLogged(%d, 1): %v", p, err)
	}
	if got != p {
		t.Fatalf("ScanLogged(%d, 1) = %d, want %d", p, got, p)
	}

	ev := requireEvent(t, logEvents(t, l), EventScan)
	if from := numField(t, ev, "from"); from != float64(p) {
		t.Errorf("port.scan from = %v, want %d", from, p)
	}
	if to := numField(t, ev, "to"); to != float64(p) {
		t.Errorf("port.scan to = %v, want %d", to, p)
	}
	if found := numField(t, ev, "found"); found != float64(p) {
		t.Errorf("port.scan found = %v, want %d", found, p)
	}
	if ev["lvl"] != "info" {
		t.Errorf("port.scan level = %v, want info", ev["lvl"])
	}
}

// TestWideScanLoggedEmitsPortScan wires the fallback scan.
func TestWideScanLoggedEmitsPortScan(t *testing.T) {
	l := testLogger(t)
	p := freePort(t)

	got, err := WideScanLogged(l, emptyDenyList(t), map[uint16]bool{p: true}, 50*time.Millisecond, testGameID)
	// The scan must not return the excluded port; if it does fail, it must
	// still have emitted the documented event with found 0.
	ev := requireEvent(t, logEvents(t, l), EventScan)
	if from := numField(t, ev, "from"); from != float64(lowestUserPort) {
		t.Errorf("port.scan from = %v, want %d", from, lowestUserPort)
	}
	if to := numField(t, ev, "to"); to != float64(reservedEphemeralLow-1) {
		t.Errorf("port.scan to = %v, want %d", to, reservedEphemeralLow-1)
	}
	if err == nil {
		if found := numField(t, ev, "found"); found != float64(got) {
			t.Errorf("port.scan found = %v, want %d", found, got)
		}
	} else if found := numField(t, ev, "found"); found != 0 {
		t.Errorf("port.scan found = %v, want 0 when the scan failed (%v)", found, err)
	}
}

// TestAvailableLoggedEmitsProbeBindFailedScanAndChange is the allocation-flow
// wiring: an occupied candidate produces port.probe, port.bind.failed, a
// port.scan for the re-entry window and port.change for the substitution.
func TestAvailableLoggedEmitsProbeBindFailedScanAndChange(t *testing.T) {
	l := testLogger(t)

	held, err := Bind(0)
	if err != nil {
		t.Fatalf("Bind(0): %v", err)
	}
	defer held.Close()
	candidate := portOf(t, held)

	d := emptyDenyList(t)
	base, span := d.DefaultWindow()

	got, ln, err := AvailableLogged(l, candidate, d, nil, 50*time.Millisecond, testGameID)
	if err != nil {
		t.Fatalf("AvailableLogged(%d): %v", candidate, err)
	}
	defer ln.Close()
	if got == candidate {
		t.Fatalf("AvailableLogged returned the occupied candidate %d", candidate)
	}
	if bound := portOf(t, ln); bound != got {
		t.Fatalf("AvailableLogged returned %d but bound %d", got, bound)
	}

	events := logEvents(t, l)

	probe := requireEvent(t, events, EventProbe)
	if port := numField(t, probe, "port"); port != float64(candidate) {
		t.Errorf("port.probe port = %v, want %d", port, candidate)
	}
	if res, _ := probe["result"].(string); res == "" {
		t.Errorf("port.probe result is empty: %v", probe)
	}
	if probe["lvl"] != "debug" {
		t.Errorf("port.probe level = %v, want debug", probe["lvl"])
	}

	bindFailed := requireEvent(t, events, EventBindFailed)
	if port := numField(t, bindFailed, "port"); port != float64(candidate) {
		t.Errorf("port.bind.failed port = %v, want %d", port, candidate)
	}
	if errno := numField(t, bindFailed, "errno"); errno == 0 {
		t.Errorf("port.bind.failed errno = 0, want the EADDRINUSE errno")
	}
	if bindFailed["lvl"] != "warn" {
		t.Errorf("port.bind.failed level = %v, want warn", bindFailed["lvl"])
	}

	scans := eventsNamed(events, EventScan)
	if len(scans) == 0 {
		t.Fatalf("port.scan was not emitted; the log holds %v", eventNames(events))
	}
	sawWindow, sawFound := false, false
	for _, ev := range scans {
		if ev["from"] == float64(base) && ev["to"] == float64(base+span-1) {
			sawWindow = true
		}
		if ev["found"] == float64(got) {
			sawFound = true
		}
	}
	if !sawWindow {
		t.Errorf("no port.scan covered the defaults window %d..%d: %v", base, base+span-1, scans)
	}
	if !sawFound {
		t.Errorf("no port.scan recorded found=%d: %v", got, scans)
	}

	change := requireEvent(t, events, EventChange)
	if old := numField(t, change, "old"); old != float64(candidate) {
		t.Errorf("port.change old = %v, want %d", old, candidate)
	}
	if newPort := numField(t, change, "new"); newPort != float64(got) {
		t.Errorf("port.change new = %v, want %d", newPort, got)
	}
	if change["lvl"] != "info" {
		t.Errorf("port.change level = %v, want info", change["lvl"])
	}
}

// --- port.deny.mismatch ----------------------------------------------------

func TestCheckDenyMismatchEmitsPortDenyMismatch(t *testing.T) {
	l := testLogger(t)
	d, err := LoadDenyList(denyListPath)
	if err != nil {
		t.Fatalf("LoadDenyList: %v", err)
	}
	const denied = uint16(4045)

	// The shell accepted a port the launcher rejects: mismatch.
	if !CheckDenyMismatch(l, denied, true, d) {
		t.Fatalf("CheckDenyMismatch(%d, shellAccepted=true) = false, want true", denied)
	}
	ev := requireEvent(t, logEvents(t, l), EventDenyMismatch)
	if port := numField(t, ev, "port"); port != float64(denied) {
		t.Errorf("port.deny.mismatch port = %v, want %d", port, denied)
	}
	if ev["lvl"] != "warn" {
		t.Errorf("port.deny.mismatch level = %v, want warn", ev["lvl"])
	}

	// Agreement in either direction is not a mismatch.
	if CheckDenyMismatch(l, denied, false, d) {
		t.Errorf("CheckDenyMismatch(%d, shellAccepted=false) = true, want false", denied)
	}
	if CheckDenyMismatch(l, 8771, true, d) {
		t.Errorf("CheckDenyMismatch(8771, shellAccepted=true) = true, want false (8771 is not denied)")
	}
	if got := len(eventsNamed(logEvents(t, l), EventDenyMismatch)); got != 1 {
		t.Errorf("port.deny.mismatch emitted %d times, want exactly 1", got)
	}
}

// --- scan deadline (FIX 8) -------------------------------------------------

func TestScanBudget(t *testing.T) {
	if got := scanBudget(0); got != 10*time.Second {
		t.Errorf("scanBudget(0) = %v, want 10s (20 x the 500ms default)", got)
	}
	if got := scanBudget(5 * time.Millisecond); got != 100*time.Millisecond {
		t.Errorf("scanBudget(5ms) = %v, want 100ms", got)
	}
	if got := scanBudget(time.Hour); got != scanDeadline {
		t.Errorf("scanBudget(time.Hour) = %v, want the cap %v", got, scanDeadline)
	}
}

// TestScanDeadlineBoundsProbing proves the overall bound without needing a
// firewall: the injected probe always burns five milliseconds, so without the
// deadline this call would run for span x 5ms. It must instead stop at the
// budget and report the documented no-port error.
func TestScanDeadlineBoundsProbing(t *testing.T) {
	var calls int
	slow := func(uint16) (ProbeResult, error) {
		calls++
		time.Sleep(5 * time.Millisecond)
		return ProbeOtherApp, nil
	}

	start := time.Now()
	_, err := scanWindow(lowestUserPort, reservedEphemeralLow-lowestUserPort, emptyDenyList(t), nil, 5*time.Millisecond, slow)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNoPort) {
		t.Fatalf("scanWindow over a slow horizon = %v, want ErrNoPort", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("scanWindow ran for %v; the %v deadline was not enforced", elapsed, scanBudget(5*time.Millisecond))
	}
	if calls == 0 {
		t.Fatal("scanWindow never probed")
	}
	if calls > 100 {
		t.Fatalf("scanWindow probed %d times; the budget is %v at 5ms per probe", calls, scanBudget(5*time.Millisecond))
	}
}
