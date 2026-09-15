package pack

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// The canonical schemas are vendored into the binary. A studio's repository
// does not have to carry them, and a schema change cannot silently fail to
// reach the tool: TestEmbeddedSchemasMatchArchitecture compares this copy with
// architecture/schemas in the source repository.
//
//go:embed schemas/*.json
var schemaFS embed.FS

// schemaBase is the $id prefix every vendored schema uses. It is unexported:
// nothing outside this package names it, and the cmd package never did.
const schemaBase = "https://kobra.games/schemas/"

// Schema kinds, named by the document they validate.
const (
	SchemaLauncherConfig  = "launcher.config.schema.json"
	SchemaEngineManifest  = "engine.manifest.schema.json"
	SchemaAssetManifest   = "asset.manifest.schema.json"
	SchemaReleaseManifest = "release.manifest.schema.json"
)

// Validator compiles the vendored schemas once and validates documents against
// them (FR-SCH-3).
type Validator struct {
	compiled map[string]*jsonschema.Schema
	log      *Logger
}

// NewValidator compiles every embedded schema.
//
// Five of them — release, asset, engine, mod and save — contain negative
// lookahead, which Go's regexp cannot compile (Packaging spec §12 A8). Rather
// than abandon schema validation, the lookahead groups are stripped at load
// time and the rule they carried is enforced in code by CheckRelPath. The
// removal is reported once per schema as pack.schema.relaxed so a reviewer can
// see exactly which patterns are no longer doing work, and every relaxed
// pattern's real rule is covered by a named semantic check.
func NewValidator(log *Logger) (*Validator, error) {
	entries, err := schemaFS.ReadDir("schemas")
	if err != nil {
		return nil, Fail(EvSchemaFail, "the embedded schemas could not be read", map[string]any{"reason": "embed"})
	}

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	// Formats such as date-time and uri are guidance for authors, not portability
	// hazards: a studio may ship any RFC 3339 string, and asserting format would
	// make the build depend on the validator's locale data.
	compiler.AssertFormat = false

	v := &Validator{compiled: map[string]*jsonschema.Schema{}, log: log}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := schemaFS.ReadFile("schemas/" + entry.Name())
		if err != nil {
			return nil, Fail(EvSchemaFail, "an embedded schema could not be read", map[string]any{"file": entry.Name()})
		}
		relaxed, removed, err := relaxPatterns(raw)
		if err != nil {
			return nil, Fail(EvSchemaFail, "an embedded schema is not valid JSON", map[string]any{"file": entry.Name()})
		}
		if removed > 0 && log != nil {
			log.Warn(EvSchemaRelaxed, map[string]any{"file": entry.Name(), "lookaheads_removed": removed})
		}

		id := idOf(raw)
		if id == "" {
			id = schemaBase + entry.Name()
		}
		if err := compiler.AddResource(id, bytes.NewReader(relaxed)); err != nil {
			return nil, Fail(EvSchemaFail, "an embedded schema could not be registered", map[string]any{"file": entry.Name()})
		}
		// Rewrite a relative $ref target as well as the root id, so the
		// document resolves whichever id a schema uses internally.
		compiled, err := compiler.Compile(id)
		if err != nil {
			return nil, Fail(EvSchemaFail, "an embedded schema could not be compiled",
				map[string]any{"file": entry.Name(), "reason": "compile"})
		}
		v.compiled[entry.Name()] = compiled
	}
	return v, nil
}

// Validate checks a decoded document against a named schema.
func (v *Validator) Validate(schemaName string, doc any) error {
	schema, ok := v.compiled[schemaName]
	if !ok {
		return Fail(EvSchemaFail, "no schema is registered under that name",
			map[string]any{"schema": schemaName, "reason": "unknown_schema"})
	}
	// Round-trip through JSON so the validator sees exactly what would be
	// written to disk, not a Go struct with unexported fields.
	raw, err := json.Marshal(doc)
	if err != nil {
		return Fail(EvSchemaFail, "a document could not be re-encoded for validation",
			map[string]any{"schema": schemaName})
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return Fail(EvSchemaFail, "a document could not be decoded for validation",
			map[string]any{"schema": schemaName})
	}
	if err := schema.Validate(generic); err != nil {
		return Fail(EvSchemaFail, "a document does not satisfy its schema: "+firstLine(err.Error()),
			map[string]any{"schema": schemaName, "reason": "validation"})
	}
	return nil
}

// idOf extracts the $id of a schema document.
func idOf(raw []byte) string {
	var doc struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return doc.ID
}

// relaxPatterns removes every negative-lookahead group from the "pattern"
// properties of a schema document and reports how many it removed.
//
// Only lookahead is touched. Anchors, classes and quantifiers are left exactly
// as authored, so a relaxed schema is still doing all the work it did before
// except for the rule that Go cannot express.
func relaxPatterns(raw []byte) ([]byte, int, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, 0, err
	}
	removed := 0
	var walk func(node any) any
	walk = func(node any) any {
		switch typed := node.(type) {
		case map[string]any:
			for key, value := range typed {
				if key == "pattern" {
					if s, ok := value.(string); ok {
						relaxed, n := stripLookaheads(s)
						removed += n
						typed[key] = relaxed
						continue
					}
				}
				typed[key] = walk(value)
			}
			return typed
		case []any:
			for i, value := range typed {
				typed[i] = walk(value)
			}
			return typed
		default:
			return node
		}
	}
	out, err := json.Marshal(walk(doc))
	if err != nil {
		return nil, 0, err
	}
	return out, removed, nil
}

// stripLookaheads deletes "(?!" ... ")" groups, honouring nesting and escapes.
func stripLookaheads(pattern string) (string, int) {
	var b strings.Builder
	removed := 0
	for i := 0; i < len(pattern); {
		if strings.HasPrefix(pattern[i:], "(?!") {
			depth := 0
			j := i + 2
			for ; j < len(pattern); j++ {
				switch pattern[j] {
				case '\\':
					j++
				case '(':
					depth++
				case ')':
					if depth == 0 {
						j++
						goto done
					}
					depth--
				}
			}
		done:
			removed++
			i = j
			continue
		}
		b.WriteByte(pattern[i])
		i++
	}
	return b.String(), removed
}

// --- semantic path rules (the code half of §12 A8) -------------------------

// RelPathPrefixes are the namespaces a file index may address. A release never
// contains and never writes data/ (FR-UPD-7), and the schema says so too.
var RelPathPrefixes = []string{"game/", "launcher/"}

// CheckRelPath enforces the rules the relaxed patterns no longer carry:
// a relative POSIX path under an allowed prefix, with no dot segments, no
// backslashes, no drive letters and no NUL bytes.
func CheckRelPath(p string, prefixes []string) error {
	switch {
	case p == "":
		return fmt.Errorf("path is empty")
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("path contains a NUL byte")
	case strings.Contains(p, `\`):
		return fmt.Errorf("path contains a backslash")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("path is absolute")
	case len(p) >= 2 && p[1] == ':':
		return fmt.Errorf("path is drive-qualified")
	}
	for _, segment := range strings.Split(p, "/") {
		switch segment {
		case "":
			return fmt.Errorf("path has an empty segment")
		case ".", "..":
			return fmt.Errorf("path has a dot segment")
		}
	}
	cleaned := path.Clean(p)
	if cleaned != p {
		return fmt.Errorf("path is not canonical")
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(p, prefix) && len(p) > len(prefix) {
			return nil
		}
	}
	return fmt.Errorf("path is not under any of %v", prefixes)
}

// CheckFileIndex validates every path in a release manifest's files[] index and
// additionally rejects data/ anywhere in the list.
func CheckFileIndex(paths []string) error {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	for _, p := range sorted {
		if err := CheckRelPath(p, RelPathPrefixes); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if strings.HasPrefix(p, "data/") {
			return fmt.Errorf("%s: a release never contains data/ (FR-UPD-7)", p)
		}
	}
	return nil
}

// firstLine trims a validator error to something a log line can carry.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
