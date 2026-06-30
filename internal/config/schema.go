package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed workflow.schema.json
var schemaJSON []byte

// compiledSchema compiles the embedded JSON Schema once. A failure here is a
// build-time defect in the embedded constant, surfaced as an error (not a panic).
var compiledSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("parse embedded schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	const id = "workflow.schema.json"
	if err := c.AddResource(id, doc); err != nil {
		return nil, fmt.Errorf("add schema resource: %w", err)
	}
	sch, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	return sch, nil
})

// validateSchema validates raw YAML config bytes against the embedded JSON
// Schema (shape only). It returns a slice of human-readable schema error
// strings (empty when valid). The second return is a non-validation failure
// (unparseable YAML, broken schema).
func validateSchema(yamlData []byte) ([]string, error) {
	sch, err := compiledSchema()
	if err != nil {
		return nil, err
	}
	inst, err := schemaInstance(yamlData)
	if err != nil {
		return nil, err
	}
	if err := sch.Validate(inst); err != nil {
		return []string{err.Error()}, nil
	}
	return nil, nil
}

// schemaInstance decodes YAML and normalizes it into the JSON value shape the
// schema validator expects (numbers as json.Number, maps as map[string]any).
func schemaInstance(yamlData []byte) (any, error) {
	var y any
	if err := yaml.Unmarshal(yamlData, &y); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	jb, err := json.Marshal(y)
	if err != nil {
		return nil, fmt.Errorf("normalize config to json: %w", err)
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(jb))
}
