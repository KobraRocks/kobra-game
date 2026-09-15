// Package sidecar owns the machine-local state directory: port.json,
// instance.lock and the logs directory (Launcher spec §6, §9).
//
// The sidecar is never authoritative for game data. Its loss is recoverable:
// a missing port.json means the deterministic default port is used, a missing
// lock is treated as absent, and a missing log is recreated (§1.2).
package sidecar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Schema identifiers for the two sidecar documents.
const (
	SchemaPortState    = "kobra.port-state/1"
	SchemaInstanceLock = "kobra.instance-lock/1"
)

// OriginHistoryCap is the §6.4 cap on remembered origins.
const OriginHistoryCap = 5

// OriginEntry is one remembered origin.
type OriginEntry struct {
	Origin   string    `json:"origin"`
	LastSeen time.Time `json:"last_seen"`
}

// State is the contents of port.json (§6.4).
type State struct {
	Schema        string        `json:"schema"`
	GameID        string        `json:"game_id"`
	Port          uint16        `json:"port"`
	Origin        string        `json:"origin"`
	OriginHistory []OriginEntry `json:"origin_history"`
	Updated       time.Time     `json:"updated"`
}

// PortFileName and LockFileName are the fixed names inside the sidecar
// directory.
const (
	PortFileName = "port.json"
	LockFileName = "instance.lock"
)

// Load reads port.json from dir. A missing file returns (nil, nil): that is the
// documented "use the deterministic default" path, not an error (§6.2).
func Load(dir string) (*State, error) {
	p := filepath.Join(dir, PortFileName)
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		// A corrupt port.json is treated as absent: the port is
		// machine-local preference, not authoritative state (§1.2).
		return nil, fmt.Errorf("port.json is not valid JSON: %w", err)
	}
	if st.Schema != SchemaPortState {
		return nil, fmt.Errorf("port.json has unexpected schema %q", st.Schema)
	}
	if st.Port < 1024 || st.Port > 65535 {
		return nil, fmt.Errorf("port.json holds an out-of-range port")
	}
	if len(st.OriginHistory) > OriginHistoryCap {
		st.OriginHistory = st.OriginHistory[len(st.OriginHistory)-OriginHistoryCap:]
	}
	return &st, nil
}

// Save writes port.json with the atomic protocol of §6.4/§15.3, because a torn
// port.json is a startup failure.
func (s *State) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	s.Schema = SchemaPortState
	s.Updated = time.Now().UTC()
	if s.Origin == "" && s.Port != 0 {
		s.Origin = Origin(s.Port)
	}
	if s.OriginHistory == nil {
		s.OriginHistory = []OriginEntry{}
	}
	if len(s.OriginHistory) > OriginHistoryCap {
		s.OriginHistory = s.OriginHistory[len(s.OriginHistory)-OriginHistoryCap:]
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return WriteAtomic(filepath.Join(dir, PortFileName), raw, 0o644)
}

// RememberOrigin appends origin to the history, de-duplicating by origin and
// truncating oldest-first to OriginHistoryCap. The previous entry is never
// deleted outright, so the user can move back to the old port to recover
// browser-stored state (FS §6.6, spec §8.6).
func (s *State) RememberOrigin(origin string) {
	if origin == "" {
		return
	}
	now := time.Now().UTC()
	kept := make([]OriginEntry, 0, len(s.OriginHistory)+1)
	for _, e := range s.OriginHistory {
		if e.Origin == origin {
			continue
		}
		kept = append(kept, e)
	}
	kept = append(kept, OriginEntry{Origin: origin, LastSeen: now})
	if len(kept) > OriginHistoryCap {
		kept = kept[len(kept)-OriginHistoryCap:]
	}
	s.OriginHistory = kept
}

// OriginHistoryStrings returns the remembered origins as plain strings, newest
// last. It is what the diagnostics payload exposes (§21.4) — origins only,
// never paths.
func (s *State) OriginHistoryStrings() []string {
	out := make([]string, 0, len(s.OriginHistory))
	for _, e := range s.OriginHistory {
		out = append(out, e.Origin)
	}
	return out
}

// Origin builds the canonical origin for a port. The origin is always the
// IPv4 loopback literal: `localhost` is never used, because §12.1 requires an
// exact Host match (FR-SRV-3).
func Origin(port uint16) string {
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}
