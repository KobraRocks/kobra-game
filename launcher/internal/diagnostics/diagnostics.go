// Package diagnostics implements the launcher's structured JSON-lines log
// (Launcher spec §21, Appendix C) plus the bounded in-memory ring buffer that
// backs GET /__kobra/diagnostics.
//
// One JSON object is written per line to <LogDir>/launcher.log. Rotation is
// in-process: when the file exceeds a threshold it is closed, the numbered
// generations are shifted (launcher.log.1 .. launcher.log.N), and the file is
// reopened. SIGHUP forces the same rotation (§2.4, §21.6).
//
// The rule that no error message may contain a filesystem path (§22.4) applies
// to every event in this package's catalogue: the fields below are identifiers,
// counts, and timestamps only.
package diagnostics

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Level is a log severity. Levels are ordered; a record is emitted when its
// level is at or above the configured minimum.
type Level int

const (
	LevelError Level = iota
	LevelWarn
	LevelInfo
	LevelDebug
)

func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	default:
		return "info"
	}
}

// ParseLevel parses one of error|warn|info|debug (case-insensitive).
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return LevelError, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "info", "":
		return LevelInfo, nil
	case "debug":
		return LevelDebug, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level %q (want error|warn|info|debug)", s)
	}
}

// RingCapacity is the number of log lines retained in memory (§21.3). It is a
// constant so the ring is a fixed allocation with no post-construction growth.
const RingCapacity = 500

// Options configures Open.
type Options struct {
	LogDir      string // directory for launcher.log; created if missing
	MaxBytes    int64  // rotate above this size (default 1 MiB)
	Generations int    // numbered files kept (default 5)
	Level       Level
	Version     string
	Commit      string
	// MirrorStderr copies lines to stderr as well. Used only for
	// debug/support runs and for the sidecar-fallback path, where there is
	// no log file to inspect afterwards.
	MirrorStderr bool
}

// Logger writes structured records to a rotating file and keeps the tail in a
// bounded ring buffer.
type Logger struct {
	mu           sync.Mutex
	f            *os.File
	path         string
	maxBytes     int64
	generations  int
	minLevel     Level
	size         int64
	ring         [RingCapacity]string
	ringIdx      int
	ringCount    int
	mirrorStderr bool

	// degraded is set when the log file could not be opened and records are
	// only reaching stderr/the ring. Surfaced in the diagnostics payload.
	degraded bool
}

// Open creates the log directory and opens the log file. If the directory or
// file cannot be created, Open returns a Logger that keeps records in the ring
// buffer and mirrors them to stderr, together with the error, so callers can
// record the degradation and still run (§6.3, §22).
func Open(opts Options) (*Logger, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 1 << 20
	}
	if opts.Generations <= 0 {
		opts.Generations = 5
	}
	l := &Logger{
		maxBytes:     opts.MaxBytes,
		generations:  opts.Generations,
		minLevel:     opts.Level,
		mirrorStderr: opts.MirrorStderr,
	}

	var openErr error
	if opts.LogDir != "" {
		if err := os.MkdirAll(opts.LogDir, 0o755); err != nil {
			openErr = fmt.Errorf("log directory unavailable: %w", err)
			l.degraded = true
		} else {
			p := filepath.Join(opts.LogDir, "launcher.log")
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if err != nil {
				openErr = fmt.Errorf("log file unavailable: %w", err)
				l.degraded = true
			} else {
				l.f = f
				l.path = p
				if st, err := f.Stat(); err == nil {
					l.size = st.Size()
				}
			}
		}
	} else {
		openErr = fmt.Errorf("no log directory configured")
		l.degraded = true
	}
	if l.degraded && !l.mirrorStderr {
		l.mirrorStderr = true
	}
	return l, openErr
}

// Degraded reports whether records are only reaching the ring buffer/stderr.
func (l *Logger) Degraded() bool { return l.degraded }

// Path returns the log file path, or "" when the file could not be opened.
// The path is never included in an event field or an API error (§22.4); it is
// for the operator-facing OS dialog and stdout only.
func (l *Logger) Path() string { return l.path }

// Level returns the active minimum level.
func (l *Logger) Level() Level { return l.minLevel }

// Enabled reports whether a record at lv would be emitted.
func (l *Logger) Enabled(lv Level) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enabledLocked(lv)
}

// enabledLocked is Enabled for a caller that already holds l.mu.
func (l *Logger) enabledLocked(lv Level) bool { return lv <= l.minLevel }

// Event writes one structured record. Fields are merged into the JSON object;
// the reserved keys t, lvl and evt are written by the logger itself and any
// caller-supplied values for them are ignored.
func (l *Logger) Event(lv Level, evt string, fields map[string]any) {
	if !l.Enabled(lv) {
		return
	}
	l.write(l.record(lv, evt, fields))
}

// record renders one structured line. It never touches l.mu, so rotateLocked
// can use it while holding the lock.
func (l *Logger) record(lv Level, evt string, fields map[string]any) string {
	rec := make(map[string]any, len(fields)+3)
	// Field order in the object is not significant to consumers, but the
	// catalogue's four example lines put the metadata first; encoding/json
	// sorts keys, which keeps the format byte-stable for tests.
	rec["t"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	rec["lvl"] = lv.String()
	rec["evt"] = evt
	for k, v := range fields {
		switch k {
		case "t", "lvl", "evt":
			continue
		}
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		// A field that cannot be marshalled must not silence the event.
		b, _ = json.Marshal(map[string]any{
			"t": rec["t"], "lvl": rec["lvl"], "evt": evt,
			"err": "unencodable_fields",
		})
	}
	line := string(b)
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	return line
}

func (l *Logger) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writeLocked(line, true)
}

// writeLocked appends one complete line. The caller holds l.mu.
//
// allowRotate is false only for the log.rotate record that rotateLocked emits:
// rotating again from inside a rotation would recurse without bound when the
// rotate record is itself larger than maxBytes.
func (l *Logger) writeLocked(line string, allowRotate bool) {
	// Ring buffer first: it is the in-memory record of what happened even if
	// the file write fails.
	l.ring[l.ringIdx] = strings.TrimRight(line, "\n")
	l.ringIdx = (l.ringIdx + 1) % RingCapacity
	if l.ringCount < RingCapacity {
		l.ringCount++
	}

	if l.mirrorStderr {
		_, _ = io.WriteString(os.Stderr, line)
	}
	if l.f == nil {
		return
	}
	if allowRotate && l.size+int64(len(line)) > l.maxBytes {
		l.rotateLocked()
		if l.f == nil {
			return
		}
	}
	n, err := l.f.WriteString(line)
	if err != nil {
		return
	}
	l.size += int64(n)
}

// Info, Warn, Error and Debug are the level-specific shorthands.
func (l *Logger) Info(evt string, fields map[string]any)  { l.Event(LevelInfo, evt, fields) }
func (l *Logger) Warn(evt string, fields map[string]any)  { l.Event(LevelWarn, evt, fields) }
func (l *Logger) Error(evt string, fields map[string]any) { l.Event(LevelError, evt, fields) }
func (l *Logger) Debug(evt string, fields map[string]any) { l.Event(LevelDebug, evt, fields) }

// ForceRotate rotates unconditionally; used by SIGHUP (§2.4, §21.6).
func (l *Logger) ForceRotate() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotateLocked()
}

// rotateLocked shifts launcher.log.N-1 -> N and launcher.log -> .1. The caller
// holds l.mu.
func (l *Logger) rotateLocked() {
	if l.path == "" {
		return
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	// Oldest generation is discarded.
	oldest := fmt.Sprintf("%s.%d", l.path, l.generations)
	_ = os.Remove(oldest)
	for i := l.generations - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", l.path, i)
		dst := fmt.Sprintf("%s.%d", l.path, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	_ = os.Rename(l.path, l.path+".1")

	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		l.degraded = true
		l.mirrorStderr = true
		return
	}
	l.f = f
	l.size = 0
	// Emit the rotation record directly. Calling l.Event here would re-enter
	// l.mu through Enabled and deadlock: every caller of rotateLocked (write,
	// ForceRotate) already holds the lock, and sync.Mutex is not reentrant.
	if l.enabledLocked(LevelInfo) {
		l.writeLocked(l.record(LevelInfo, "log.rotate", nil), false)
	}
}

// Tail returns up to n most recent log lines, oldest first. It is what the
// diagnostics payload's log_tail field is built from.
func (l *Logger) Tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > l.ringCount {
		n = l.ringCount
	}
	return l.tailLocked(n)
}

// tailLocked renders the n most recent retained lines. The caller holds l.mu.
func (l *Logger) tailLocked(n int) []string {
	out := make([]string, 0, n)
	start := (l.ringIdx - n + RingCapacity) % RingCapacity
	for i := 0; i < n; i++ {
		out = append(out, l.ring[(start+i)%RingCapacity])
	}
	return out
}

// WriteStackFile writes a panic stack dump to <LogDir>/crash-<ts>.txt and
// returns the file's base name (never a full path — §22.4). An error means the
// dump could not be written; the baseline crash-<ts>.txt name is still
// returned so the event has a stable value.
func (l *Logger) WriteStackFile(stack []byte) (string, error) {
	name := fmt.Sprintf("crash-%s.txt", time.Now().UTC().Format("20060102T150405Z"))
	dir := ""
	if l.path != "" {
		dir = filepath.Dir(l.path)
	}
	if dir == "" {
		return name, fmt.Errorf("no log directory available for crash dump")
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, stack, 0o644); err != nil {
		return name, err
	}
	return name, nil
}

// WatchSIGHUP installs a SIGHUP handler that forces log rotation. It returns a
// stop function. On platforms without SIGHUP the goroutine exits immediately.
func (l *Logger) WatchSIGHUP() (stop func()) {
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ch:
				l.ForceRotate()
			case <-done:
				signal.Stop(ch)
				return
			}
		}
	}()
	return func() { close(done) }
}

// Close flushes and closes the log file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
