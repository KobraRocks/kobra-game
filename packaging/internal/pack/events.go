// Package pack implements the Kobra packaging pipeline of
// architecture/Packaging-spec.md §5.1.
//
// It is a publisher-side tool. It is deliberately NOT part of the launcher
// module: the launcher ships to users and keeps a three-dependency footprint
// (Launcher spec §3.3), while the packager runs on a studio's machine and may
// carry a schema validator and a TOML parser. Nothing in this package is ever
// placed in a package.
package pack

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
)

// Level is an event severity. The vocabulary is fixed by the launcher's log
// discipline (Launcher spec §21) so that a studio's CI can treat packager
// output the same way it treats launcher output.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Event names are normative (Packaging spec Appendix B). A gate asserts on a
// name, never on prose, so a message can be reworded without breaking CI.
const (
	EvStart          = "pack.start"
	EvIdentityOK     = "pack.identity.ok"
	EvIdentityFail   = "pack.identity.fail"
	EvSchemaRelaxed  = "pack.schema.relaxed"
	EvSchemaFail     = "pack.schema.fail"
	EvScanReject     = "pack.scan.reject"
	EvManifestGen    = "pack.manifest.generated"
	EvArchiveBuilt   = "pack.archive.built"
	EvHash           = "pack.hash"
	EvSignSkipped    = "pack.sign.skipped"
	EvSign           = "pack.sign"
	EvPublish        = "pack.publish"
	EvVerifyOK       = "pack.verify.ok"
	EvVerifyFail     = "pack.verify.fail"
	EvCheckOK        = "pack.check.ok"
	EvDone           = "pack.done"
	EvReadmeUnstable = "pack.readme.unstable"
)

// Logger emits the structured event stream. It is the tool's only output
// channel, so a CI job parses one thing: either "event=... key=value" lines or
// one JSON object per line.
type Logger struct {
	mu     sync.Mutex
	w      io.Writer
	asJSON bool
}

// NewLogger builds a logger. jsonMode emits one JSON object per line, which is
// what an automated consumer should use.
func NewLogger(w io.Writer, jsonMode bool) *Logger {
	return &Logger{w: w, asJSON: jsonMode}
}

// Emit writes one event. Field order is deterministic in text mode so that two
// builds of the same inputs produce the same log.
func (l *Logger) Emit(level Level, event string, fields map[string]any) {
	if l == nil || l.w == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.asJSON {
		doc := make(map[string]any, len(fields)+2)
		for k, v := range fields {
			doc[k] = v
		}
		doc["level"] = string(level)
		doc["event"] = event
		raw, err := json.Marshal(doc)
		if err != nil {
			fmt.Fprintf(l.w, "{\"level\":\"error\",\"event\":\"pack.log.marshal\"}\n")
			return
		}
		l.w.Write(append(raw, '\n'))
		return
	}

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(l.w, "[%-5s] %s", level, event)
	for _, k := range keys {
		fmt.Fprintf(l.w, " %s=%v", k, fields[k])
	}
	fmt.Fprintln(l.w)
}

func (l *Logger) Info(event string, fields map[string]any)  { l.Emit(LevelInfo, event, fields) }
func (l *Logger) Warn(event string, fields map[string]any)  { l.Emit(LevelWarn, event, fields) }
func (l *Logger) Error(event string, fields map[string]any) { l.Emit(LevelError, event, fields) }

// BuildError is a blocking packaging failure: "fail the build, never the user"
// (Packaging spec §1.4 principle 5). It always carries the event that classifies
// it, so a caller reports structure and not just a sentence.
type BuildError struct {
	Event  string
	Fields map[string]any
	Msg    string
}

func (e *BuildError) Error() string {
	if e.Event == "" {
		return e.Msg
	}
	return fmt.Sprintf("%s: %s", e.Event, e.Msg)
}

// Fail builds a classified error.
func Fail(event, msg string, fields map[string]any) *BuildError {
	return &BuildError{Event: event, Fields: fields, Msg: msg}
}
