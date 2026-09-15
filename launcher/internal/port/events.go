package port

import (
	"errors"
	"syscall"
	"time"

	"kobragames.local/launcher/internal/diagnostics"
)

// Appendix C.2 event names. The core of this package is pure — it takes no
// logger and writes nothing — so these helpers exist to let the caller emit the
// documented event, with the documented fields, in one place. They take the
// *diagnostics.Logger explicitly: this package never holds a global logger.
const (
	EventCandidate    = "port.candidate"     // info: port, source (saved/default/scan/user)
	EventProbe        = "port.probe"         // debug: port, result (free/ours/other)
	EventConfirmed    = "port.confirmed"     // info: port, user_edited
	EventBindFailed   = "port.bind.failed"   // warn: port, errno
	EventScan         = "port.scan"          // info: from, to, found
	EventDenyMismatch = "port.deny.mismatch" // warn: port
	EventChange       = "port.change"        // info: old, new
)

// LogCandidate emits port.candidate {port, source}.
func LogCandidate(l *diagnostics.Logger, p uint16, src Source) {
	if l == nil {
		return
	}
	l.Info(EventCandidate, map[string]any{"port": p, "source": string(src)})
}

// LogProbe emits port.probe {port, result}. The result token is ProbeResult's
// String; ProbeOtherGame and ProbeOtherApp both read as "other" to the
// documented vocabulary only in the sense that neither is free or ours.
func LogProbe(l *diagnostics.Logger, p uint16, r ProbeResult) {
	if l == nil {
		return
	}
	l.Debug(EventProbe, map[string]any{"port": p, "result": r.String()})
}

// ProbeLogged is Probe plus the port.probe event {port, result}. The bare probe
// is used inside allocation, where AvailableLogged already emits the event;
// this helper is for a caller that probes one port and reports it, such as the
// --check-port fast path.
func ProbeLogged(l *diagnostics.Logger, p uint16, timeout time.Duration, oursGameID string) (ProbeResult, error) {
	res, err := Probe(p, timeout, oursGameID)
	LogProbe(l, p, res)
	return res, err
}

// LogConfirmed emits port.confirmed {port, user_edited}.
func LogConfirmed(l *diagnostics.Logger, p uint16, userEdited bool) {
	if l == nil {
		return
	}
	l.Info(EventConfirmed, map[string]any{"port": p, "user_edited": userEdited})
}

// LogBindFailed emits port.bind.failed {port, errno}. The errno is the numeric
// syscall error when err carries one, else 0.
func LogBindFailed(l *diagnostics.Logger, p uint16, err error) {
	if l == nil {
		return
	}
	var errno syscall.Errno
	if err == nil || !errors.As(err, &errno) {
		errno = 0
	}
	l.Warn(EventBindFailed, map[string]any{"port": p, "errno": int(errno)})
}

// LogScan emits port.scan {from, to, found}. found is the port the scan
// settled on, or 0 when the window was exhausted.
func LogScan(l *diagnostics.Logger, from, to, found uint16) {
	if l == nil {
		return
	}
	l.Info(EventScan, map[string]any{"from": from, "to": to, "found": found})
}

// LogDenyMismatch emits port.deny.mismatch {port} when the shell's copy of the
// deny list disagrees with the launcher's (the launcher's decision is
// authoritative, §8.4).
func LogDenyMismatch(l *diagnostics.Logger, p uint16) {
	if l == nil {
		return
	}
	l.Warn(EventDenyMismatch, map[string]any{"port": p})
}

// CheckDenyMismatch compares the shell's verdict about p with the launcher's
// authoritative deny list. The shell ships its own copy of the list (§8.4), so
// a stale copy can accept a port the launcher rejects — the dangerous
// direction, because the shell then proposes a port that fails to bind. When
// that happens the mismatch is logged as port.deny.mismatch and true is
// returned; the launcher's decision still wins, so callers answer with
// Contains/Validate, never with shellAccepted.
//
// It is the call site LogDenyMismatch was missing: it belongs wherever a
// shell-proposed or user-edited port is checked.
func CheckDenyMismatch(l *diagnostics.Logger, p uint16, shellAccepted bool, d *DenyList) bool {
	if !shellAccepted || !d.Contains(p) {
		return false
	}
	LogDenyMismatch(l, p)
	return true
}

// LogChange emits port.change {old, new} when an allocation substitutes a
// different port for the proposed one.
func LogChange(l *diagnostics.Logger, oldPort, newPort uint16) {
	if l == nil {
		return
	}
	l.Info(EventChange, map[string]any{"old": oldPort, "new": newPort})
}
