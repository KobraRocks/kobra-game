package port

import (
	"time"

	"kobragames.local/launcher/internal/diagnostics"
)

// reservedEphemeralLow is the first port of the reserved ephemeral range
// (§8.3, §8.4). Ports at or above it are never allocated automatically.
const reservedEphemeralLow = 49152

// lowestUserPort is the first non-privileged port.
const lowestUserPort = 1024

// scanDeadline caps the total wall-clock time one Scan or WideScan may spend
// probing. Probing is advisory and Bind is authoritative (§8.3), so a scan that
// has burned this much time cannot be trusted to finish soon: it stops and
// reports ErrNoPort, and the caller's bind decides. Without the cap, a firewall
// that silently drops loopback SYN or HTTP responses makes each of the ~47 000
// candidate ports cost a full probe timeout, turning startup into a multi-hour
// hang.
const scanDeadline = 15 * time.Second

// scanDeadlineFactor is how many probe timeouts one scan may spend before the
// scanDeadline cap applies. Keeping the budget proportional to the caller's
// timeout means a scan given a deliberately short timeout (tests, --check-port)
// also gets a short overall bound, while the default timeout gets a few seconds.
const scanDeadlineFactor = 20

// scanBudget returns the overall wall-clock budget for one Scan or WideScan.
//
// A non-positive timeout means DefaultProbeTimeout. The budget is
// scanDeadlineFactor probe timeouts, capped by scanDeadline: 10s for the
// default 500ms timeout, 15s for anything at or above 750ms. The budget bounds
// how many probes a scan may start; it cannot interrupt a single probe already
// in flight, because Probe has no context.
func scanBudget(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	budget := time.Duration(scanDeadlineFactor) * timeout
	if budget > scanDeadline {
		return scanDeadline
	}
	return budget
}

// candidateUsable is the pre-probe filter shared by Scan and WideScan: the port
// is inside the user range, is not excluded by the caller, and is neither
// hard-denied nor reserved.
func candidateUsable(p uint16, d *DenyList, exclude map[uint16]bool) bool {
	if exclude[p] {
		return false
	}
	if p < lowestUserPort || p >= reservedEphemeralLow {
		return false
	}
	if d.Contains(p) || d.Reserved(p) {
		return false
	}
	return true
}

// probeFunc is the probing seam shared by Scan and WideScan. Its only purpose
// is to let the overall deadline be exercised deterministically without a
// network; production always passes realProbe.
type probeFunc func(p uint16) (ProbeResult, error)

// realProbe binds a probeFunc to a timeout and the launcher's game id.
func realProbe(timeout time.Duration, oursGameID string) probeFunc {
	return func(p uint16) (ProbeResult, error) { return Probe(p, timeout, oursGameID) }
}

// Scan walks [base, base+span-1] and returns the first candidate that is not
// denied and whose probe says free or ours (§8.3). exclude holds ports to skip
// — for example an existing instance of the same game in another folder
// (§9.5) — and may be nil.
//
// The scan is advisory: the caller must still Bind the returned port, because
// another process can take it between the probe and the bind (the TOCTOU
// window §8.3). ErrNoPort means the window is exhausted, or that the overall
// deadline (scanDeadline) elapsed; the caller widens with WideScan.
func Scan(base, span uint16, d *DenyList, exclude map[uint16]bool, timeout time.Duration, oursGameID string) (uint16, error) {
	return scanWindow(base, span, d, exclude, timeout, realProbe(timeout, oursGameID))
}

// ScanLogged is Scan plus the Appendix C.2 port.scan event {from, to, found}.
// It exists for a caller that owns a *diagnostics.Logger, because this package
// never holds one.
func ScanLogged(l *diagnostics.Logger, base, span uint16, d *DenyList, exclude map[uint16]bool, timeout time.Duration, oursGameID string) (uint16, error) {
	p, err := Scan(base, span, d, exclude, timeout, oursGameID)
	LogScan(l, base, base+span-1, scanFound(p, err))
	return p, err
}

// WideScan is the §8.3 fallback: it walks the whole non-privileged,
// non-ephemeral range 1024..49151, excluding the deny set, the reserved
// ranges, and the caller's exclude set, and returns the first port whose probe
// says free or ours. It returns ErrNoPort when nothing is available, including
// when the overall deadline (scanDeadline) elapses first.
func WideScan(d *DenyList, exclude map[uint16]bool, timeout time.Duration, oursGameID string) (uint16, error) {
	return scanWindow(lowestUserPort, reservedEphemeralLow-lowestUserPort, d, exclude, timeout, realProbe(timeout, oursGameID))
}

// WideScanLogged is WideScan plus the Appendix C.2 port.scan event {from, to,
// found}.
func WideScanLogged(l *diagnostics.Logger, d *DenyList, exclude map[uint16]bool, timeout time.Duration, oursGameID string) (uint16, error) {
	p, err := WideScan(d, exclude, timeout, oursGameID)
	LogScan(l, lowestUserPort, reservedEphemeralLow-1, scanFound(p, err))
	return p, err
}

// scanWindow is the one scan loop. It probes base..base+span-1 (bounded by the
// uint16 port range and by scanBudget(timeout)) and returns the first usable
// port, or ErrNoPort.
func scanWindow(base, span uint16, d *DenyList, exclude map[uint16]bool, timeout time.Duration, probe probeFunc) (uint16, error) {
	if span == 0 {
		return 0, ErrNoPort
	}
	deadline := time.Now().Add(scanBudget(timeout))
	for i := uint32(0); i < uint32(span); i++ {
		if time.Now().After(deadline) {
			return 0, ErrNoPort
		}
		p32 := uint32(base) + i
		if p32 > 65535 {
			break
		}
		p := uint16(p32)
		if !candidateUsable(p, d, exclude) {
			continue
		}
		res, err := probe(p)
		if err != nil || !res.Usable() {
			continue
		}
		return p, nil
	}
	return 0, ErrNoPort
}

// scanFound renders the port.scan "found" field: the port the scan settled on,
// or 0 when it did not settle on one.
func scanFound(p uint16, err error) uint16 {
	if err != nil {
		return 0
	}
	return p
}
