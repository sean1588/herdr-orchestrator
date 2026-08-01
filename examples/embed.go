// Package examples ships the default workflow config and its rubric prompts
// as an embedded filesystem, so `orchestratord init` scaffolds the same files
// a user can read here.
package examples

import "embed"

// FS holds default-pipeline.yaml and prompts/*.md.
//
//go:embed default-pipeline.yaml prompts
var FS embed.FS

// Placeholder tokens init substitutes in default-pipeline.yaml. Exact-token
// string replacement, not YAML rewriting — it keeps the file's comments.
const (
	PlaceholderRepo = "your-github-user/your-repo"
	DefaultLabel    = "agent-ready"
)
