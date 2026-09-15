// Package port implements the launcher's port allocation subsystem
// (Launcher spec §7.3, §8, §9.3, Appendix A.3).
//
// The package is deliberately pure: no function in it writes to a log. The
// Appendix C.2 events are emitted by the caller, or by the optional Log*
// helpers in events.go, which take the *diagnostics.Logger explicitly. There
// is no package-level logger and no mutable package-level state, so the
// package can be exercised deterministically from tests.
//
// Two rules from the spec shape the whole package:
//
//   - Probing is advisory; binding is authoritative (§8.3). Scan and WideScan
//     only *propose* a port; Bind is the operation that actually claims it.
//   - No user-facing message may contain a filesystem path (§22.4). DenyList
//     messages are therefore fixed strings generated from the shipped data,
//     never the path the data was loaded from.
package port

import (
	"encoding/json"
	"fmt"
	"os"
)

// DenyListSchema is the schema identifier the shipped port-deny-list.json
// carries. A file with any other value is rejected rather than half-applied.
const DenyListSchema = "kobra.port-deny-list/1"

// DenyList is the parsed port-deny-list.json (FS Appendix B), reduced to two
// lookups: the individually denied ports and the denied ranges. Both are
// consulted by Contains/Reason.
//
// The reserved_ranges block is kept separately. It is not part of the hard
// deny set: §8.4's Validate must still report ErrEphemeralPort for the
// reserved 49152–65535 range, which it can only do if Contains stays silent
// about it. Scan/WideScan skip reserved ports as well (the schema says the
// launcher "warns and refuses by default").
type DenyList struct {
	ports    map[uint16]string
	ranges   []denyRange
	reserved []denyRange

	// base and span are the file's defaults block, used only by Available
	// when it has to re-enter allocation and has no configured window
	// (see Available's doc comment).
	base uint16
	span uint16
}

type denyRange struct {
	from uint16
	to   uint16
	msg  string
}

func (r denyRange) contains(p uint16) bool { return p >= r.from && p <= r.to }

// Wire shapes of port-deny-list.json. Only the fields the launcher consumes
// are decoded; the descriptive fields (description, generated, source) are
// ignored on purpose.
type denyListFile struct {
	Schema         string          `json:"schema"`
	Ranges         []denyListRange `json:"ranges"`
	Ports          []denyListPort  `json:"ports"`
	ReservedRanges []denyListRange `json:"reserved_ranges"`
	Defaults       denyListDefault `json:"defaults"`
}

type denyListRange struct {
	From    uint16 `json:"from"`
	To      uint16 `json:"to"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type denyListPort struct {
	Port    uint16 `json:"port"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type denyListDefault struct {
	Base uint16 `json:"base"`
	Span uint16 `json:"span"`
}

// LoadDenyList reads and parses the shared deny list. The file is loaded once
// at startup (§7.3); callers hold the returned *DenyList for the process
// lifetime. A nil *DenyList is valid and denies nothing, which keeps tests and
// the "no deny list configured" path simple.
func LoadDenyList(path string) (*DenyList, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		// The path is included in the cause so an operator can find the
		// file; this error never reaches the browser or a DenyList reason
		// (§22.4).
		return nil, fmt.Errorf("port deny list could not be read: %w", err)
	}
	d, err := ParseDenyList(b)
	if err != nil {
		return nil, fmt.Errorf("port deny list is not usable: %w", err)
	}
	return d, nil
}

// ParseDenyList parses the bytes of a port-deny-list.json document. It is
// exported so the shared schema's shape can be exercised without touching the
// filesystem.
func ParseDenyList(data []byte) (*DenyList, error) {
	var f denyListFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if f.Schema != DenyListSchema {
		return nil, fmt.Errorf("schema must be %q, got %q", DenyListSchema, f.Schema)
	}
	d := &DenyList{
		ports: make(map[uint16]string, len(f.Ports)),
		base:  f.Defaults.Base,
		span:  f.Defaults.Span,
	}
	for _, r := range f.Ranges {
		if r.From == 0 || r.To == 0 || r.From > r.To {
			return nil, fmt.Errorf("invalid denied range %d-%d", r.From, r.To)
		}
		d.ranges = append(d.ranges, denyRange{from: r.From, to: r.To, msg: rangeMessage(r.From, r.To, r.Message)})
	}
	for _, r := range f.ReservedRanges {
		if r.From == 0 || r.To == 0 || r.From > r.To {
			return nil, fmt.Errorf("invalid reserved range %d-%d", r.From, r.To)
		}
		d.reserved = append(d.reserved, denyRange{from: r.From, to: r.To, msg: rangeMessage(r.From, r.To, r.Message)})
	}
	for _, p := range f.Ports {
		if p.Port == 0 {
			return nil, fmt.Errorf("invalid denied port 0")
		}
		msg := p.Message
		if msg == "" {
			msg = fmt.Sprintf("Port %d is blocked by browsers or reserved by the operating system. Pick another port.", p.Port)
		}
		d.ports[p.Port] = msg
	}
	return d, nil
}

func rangeMessage(from, to uint16, msg string) string {
	if msg != "" {
		return msg
	}
	return fmt.Sprintf("Ports %d-%d are blocked by browsers or reserved by the operating system. Pick another port.", from, to)
}

// Contains reports whether p is hard-denied: it is listed individually, or it
// falls inside a denied range. It is the predicate §8.4's Validate uses, and
// it deliberately ignores reserved_ranges so that ErrEphemeralPort still fires
// for 49152+ (the shipped reserved range).
//
// A nil *DenyList contains nothing.
func (d *DenyList) Contains(p uint16) bool {
	if d == nil {
		return false
	}
	if _, ok := d.ports[p]; ok {
		return true
	}
	for _, r := range d.ranges {
		if r.contains(p) {
			return true
		}
	}
	return false
}

// Reserved reports whether p falls in a reserved_ranges entry (the ephemeral
// range in the shipped file). Reserved ports are not hard-denied — Validate
// reports ErrEphemeralPort for the ones the spec names — but allocation skips
// them, because the schema says the launcher "warns and refuses by default".
func (d *DenyList) Reserved(p uint16) bool {
	if d == nil {
		return false
	}
	for _, r := range d.reserved {
		if r.contains(p) {
			return true
		}
	}
	return false
}

// Reason returns the user-facing explanation for why p is unusable, or "" when
// it is usable. The message comes from the shipped file's "message" field when
// present, else from a fixed generated sentence. It never contains a
// filesystem path (§22.4), which is why it is safe to print.
//
// Reason also answers for reserved ranges, even though Contains returns false
// for them, so callers that need the ephemeral explanation have one source.
func (d *DenyList) Reason(p uint16) string {
	if d == nil {
		return ""
	}
	if msg, ok := d.ports[p]; ok {
		return msg
	}
	for _, r := range d.ranges {
		if r.contains(p) {
			return r.msg
		}
	}
	for _, r := range d.reserved {
		if r.contains(p) {
			return r.msg
		}
	}
	return ""
}

// DefaultWindow returns the base and span from the file's defaults block
// (8765/100 in the shipped file). It exists because Available must re-enter
// allocation without a configured window; see Available.
func (d *DenyList) DefaultWindow() (base, span uint16) {
	if d == nil {
		return 0, 0
	}
	return d.base, d.span
}

// Len reports the number of individually denied ports. Ranges are not counted.
func (d *DenyList) Len() int {
	if d == nil {
		return 0
	}
	return len(d.ports)
}
