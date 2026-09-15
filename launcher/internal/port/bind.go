package port

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"kobragames.local/launcher/internal/diagnostics"
)

// Bind is the authoritative availability test (§8.3): it binds
// 127.0.0.1:<p> on tcp4 and returns the listener. A successful Bind means the
// port is ours; no probe result can override that.
//
// tcp4 is explicit so only the IPv4 loopback is bound (FR-SRV-3, §10.1). The
// socket gets SO_REUSEADDR on POSIX — so a TIME_WAIT listener from a previous
// launch does not block a restart — but never SO_REUSEPORT, so a second live
// bind on the same port fails with EADDRINUSE instead of silently sharing it.
// On Windows the socket option is a no-op; see reuseaddr_windows.go.
//
// p == 0 asks the OS for a free port (the caller can read it from
// listener.Addr()). The returned listener is open; the caller owns it and must
// close it.
func Bind(p uint16) (net.Listener, error) {
	lc := net.ListenConfig{Control: controlReuseAddr}
	return lc.Listen(context.Background(), "tcp4", fmt.Sprintf("127.0.0.1:%d", p))
}

// isAddrInUse reports whether err is EADDRINUSE, the one bind failure that
// means "try another port" rather than "something is wrong" (§8.3).
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// Available runs the §8.3 flow for one candidate:
//
//  1. probe the candidate (advisory — a probe failure never rejects it),
//  2. bind it; on success the port is taken and returned,
//  3. on EADDRINUSE only, re-enter allocation exactly once: Scan the window,
//     then WideScan; the first port that also binds is returned.
//
// Any other bind error (no permission, bad address) is returned as-is, because
// retrying cannot help.
//
// The window comes from the deny list's defaults block, because this signature
// has no configured base/span and the spec's window is
// config.Port.Base..Base+Span. When the deny list carries no defaults, step 3
// goes straight to WideScan. The candidate itself is excluded from the retry,
// so a bind failure cannot hand the same port straight back.
//
// Returns ErrNoPort when the single re-entry finds nothing.
func Available(candidate uint16, d *DenyList, exclude map[uint16]bool, timeout time.Duration, oursGameID string) (uint16, net.Listener, error) {
	return available(nil, candidate, d, exclude, timeout, oursGameID)
}

// AvailableLogged is Available plus the Appendix C.2 events the allocation flow
// owns: port.probe for the advisory probe of the candidate, port.bind.failed
// when the authoritative bind fails, port.scan for each scan the single
// re-entry runs, and port.change when a different port is substituted for the
// proposed one.
//
// It exists for a caller that owns a *diagnostics.Logger, because this package
// never holds one. Available keeps its signature and emits nothing.
func AvailableLogged(l *diagnostics.Logger, candidate uint16, d *DenyList, exclude map[uint16]bool, timeout time.Duration, oursGameID string) (uint16, net.Listener, error) {
	return available(l, candidate, d, exclude, timeout, oursGameID)
}

// available is the one implementation behind Available and AvailableLogged.
// A nil logger silences every event.
func available(l *diagnostics.Logger, candidate uint16, d *DenyList, exclude map[uint16]bool, timeout time.Duration, oursGameID string) (uint16, net.Listener, error) {
	probe := realProbe(timeout, oursGameID)

	// Step 1: advisory probe. The error is intentionally dropped; the
	// authoritative answer is the bind below.
	res, _ := probe(candidate)
	LogProbe(l, candidate, res)

	// Step 2: authoritative bind.
	lst, err := Bind(candidate)
	if err == nil {
		return candidate, lst, nil
	}
	LogBindFailed(l, candidate, err)
	if !isAddrInUse(err) {
		return 0, nil, err
	}

	// Step 3: one re-entry into allocation (FR-SRV-23).
	skip := make(map[uint16]bool, len(exclude)+1)
	for k, v := range exclude {
		skip[k] = v
	}
	skip[candidate] = true

	if base, span := d.DefaultWindow(); span > 0 {
		p, serr := scanWindow(base, span, d, skip, timeout, probe)
		LogScan(l, base, base+span-1, scanFound(p, serr))
		if serr == nil {
			if l2, berr := Bind(p); berr == nil {
				LogChange(l, candidate, p)
				return p, l2, nil
			}
		}
	}
	p, serr := WideScan(d, skip, timeout, oursGameID)
	LogScan(l, lowestUserPort, reservedEphemeralLow-1, scanFound(p, serr))
	if serr == nil {
		if l2, berr := Bind(p); berr == nil {
			LogChange(l, candidate, p)
			return p, l2, nil
		}
	}
	return 0, nil, ErrNoPort
}
