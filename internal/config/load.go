package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load reads, schema-validates, decodes, and invariant-checks a workflow config
// file. It returns the decoded workflow, any non-fatal warnings, and an error
// that aggregates schema violations or invariant failures (nil if valid).
//
// A workflow that fails validation must not be run. Warnings are advisory and
// returned regardless of whether the config is valid.
func Load(path string) (*Workflow, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config %q: %w", path, err)
	}
	return parse(data)
}

// parse runs the same pipeline as Load on in-memory bytes.
func parse(data []byte) (*Workflow, []string, error) {
	schemaErrs, err := validateSchema(data)
	if err != nil {
		return nil, nil, fmt.Errorf("schema validation: %w", err)
	}
	if len(schemaErrs) > 0 {
		return nil, nil, &ValidationErrors{Stage: "schema", Errors: schemaErrs}
	}

	var wf Workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return nil, nil, fmt.Errorf("decode config: %w", err)
	}

	warnings, semErrs := semanticChecks(&wf)
	if len(semErrs) > 0 {
		return &wf, warnings, &ValidationErrors{Stage: "semantic", Errors: semErrs}
	}
	return &wf, warnings, nil
}

// ValidationErrors is the aggregated result of a failed validation stage.
type ValidationErrors struct {
	Stage  string // "schema" or "semantic"
	Errors []string
}

func (e *ValidationErrors) Error() string {
	return fmt.Sprintf("%s validation failed: %d error(s)\n  %s",
		e.Stage, len(e.Errors), strings.Join(e.Errors, "\n  "))
}
