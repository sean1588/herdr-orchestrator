package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// UnmarshalYAML lets a gate reference be written as a scalar ("pr_exists") or a
// sequence (["ci_green", "approvals"]). Both decode to a GateRef slice.
func (g *GateRef) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*g = GateRef{node.Value}
		return nil
	}
	var arr []string
	if err := node.Decode(&arr); err != nil {
		return fmt.Errorf("gate reference must be a string or list of strings: %w", err)
	}
	*g = GateRef(arr)
	return nil
}
