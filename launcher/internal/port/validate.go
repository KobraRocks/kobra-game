package port

import (
	"errors"
	"fmt"
)

// Sentinel validation errors (§8.4). They are exported so the CLI can map them
// to their own exit codes and messages with errors.Is.
var (
	// ErrPrivilegedPort: p < 1024. Binding would need privileges and the
	// OS may have reserved the port.
	ErrPrivilegedPort = errors.New("ports below 1024 are reserved for system services")
	// ErrEphemeralPort: p >= 49152. The OS may claim it at any moment.
	ErrEphemeralPort = errors.New("ports 49152 and above are used automatically by the operating system")
	// ErrNoPort: allocation exhausted its search space (§8.3).
	ErrNoPort = errors.New("no usable port could be found")
)

// DenyError reports that a port is on the shared deny list. It carries the
// user-facing Reason so the CLI can explain the rejection without re-deriving
// it, and so Contains/Validate can never disagree about why a port failed.
type DenyError struct {
	Port   uint16
	Reason string
}

func (e *DenyError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("port %d is not usable", e.Port)
	}
	return fmt.Sprintf("port %d is not usable: %s", e.Port, e.Reason)
}

// Validate is the §8.4 check that runs on every candidate, whatever its source:
// the sidecar, the deterministic default, a scan, or the user.
//
//	switch {
//	case p < 1024:        return ErrPrivilegedPort
//	case deny.Contains(p): return &DenyError{...}   // carries Reason(p)
//	case p >= 49152:      return ErrEphemeralPort
//	default:              return nil
//	}
//
// A nil *DenyList denies nothing. Note the ordering: the privileged and
// ephemeral sentinels are the spec's, and the deny list is consulted in
// between; Contains deliberately excludes the file's reserved_ranges so the
// 49152+ check still fires for it.
func Validate(p uint16, d *DenyList) error {
	switch {
	case p < 1024:
		return ErrPrivilegedPort
	case d.Contains(p):
		return &DenyError{Port: p, Reason: d.Reason(p)}
	case p >= 49152:
		return ErrEphemeralPort
	default:
		return nil
	}
}
