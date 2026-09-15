package config

import (
	"bytes"
	"embed"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

//go:embed schema/launcher.config.schema.json
var schemaFS embed.FS

const (
	// SchemaID is the config schema's $id.
	SchemaID = "https://kobra.games/schemas/launcher.config.schema.json"
	// ExpectedSchema is the required value of the "schema" property.
	ExpectedSchema = "kobra.launcher-config/1"
)

var (
	schemaOnce sync.Once
	schemaVal  *jsonschema.Schema
	schemaErr  error
)

// Schema returns the compiled launcher config schema. It is built once; the
// schema is a build-time asset, so a failure here is a build error rather than
// a user error.
func Schema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		data, err := schemaFS.ReadFile("schema/launcher.config.schema.json")
		if err != nil {
			schemaErr = fmt.Errorf("embedded schema missing: %w", err)
			return
		}
		c := jsonschema.NewCompiler()
		c.Draft = jsonschema.Draft2020
		// Format assertions (date-time, uri) are not part of the launcher's
		// contract; the schema uses them descriptively.
		c.AssertFormat = false
		if err := c.AddResource(SchemaID, bytes.NewReader(data)); err != nil {
			schemaErr = err
			return
		}
		schemaVal, schemaErr = c.Compile(SchemaID)
	})
	return schemaVal, schemaErr
}

// Validate checks raw against the embedded launcher config schema. It is the
// FR-SCH-3 gate: no config, no startup.
func Validate(raw []byte) error {
	sch, err := Schema()
	if err != nil {
		return err
	}
	doc, err := decodeJSONBytes(raw)
	if err != nil {
		return fmt.Errorf("launcher.config.json is not valid JSON: %w", err)
	}
	if err := sch.Validate(doc); err != nil {
		return fmt.Errorf("launcher.config.json does not match %s: %s", ExpectedSchema, firstSchemaError(err))
	}
	return nil
}

// firstSchemaError renders one line of a validation failure. The library's
// message names only schema keywords and JSON pointers, never filesystem
// paths, so it is safe in the operator log (§7.1). It is never placed in an
// API response.
func firstSchemaError(err error) string {
	if ve, ok := err.(*jsonschema.ValidationError); ok {
		for len(ve.Causes) > 0 {
			ve = ve.Causes[0]
		}
		if ve.InstanceLocation != "" {
			return fmt.Sprintf("%s: %s", ve.InstanceLocation, ve.Message)
		}
		return ve.Message
	}
	return err.Error()
}
