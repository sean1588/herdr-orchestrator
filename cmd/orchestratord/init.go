package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean1588/herdr-orchestrator/examples"
)

// cmdInit scaffolds a working config from the embedded example: writes
// <dir>/pipeline.yaml + <dir>/prompts/ with the target repo (and optionally
// the source label) substituted. Filesystem-only — creating the label in the
// target repo stays a `gh label create` step. Exit 0 on success, 1 if any
// target file already exists (nothing is written), 2 on usage errors.
func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	repo := fs.String("repo", "", "target GitHub repo as owner/name (required)")
	label := fs.String("label", examples.DefaultLabel, "source label the daemon polls")
	dir := fs.String("dir", ".", "directory to scaffold into")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if owner, name, ok := strings.Cut(*repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		fmt.Fprintln(os.Stderr, "init: --repo must be owner/name")
		return 2
	}

	files := map[string]string{
		"default-pipeline.yaml": "pipeline.yaml",
		"prompts/triage.md":     "prompts/triage.md",
		"prompts/review.md":     "prompts/review.md",
	}
	for _, dst := range files {
		if _, err := os.Stat(filepath.Join(*dir, dst)); err == nil {
			fmt.Fprintf(os.Stderr, "init: %s already exists; refusing to overwrite\n", filepath.Join(*dir, dst))
			return 1
		}
	}

	for src, dst := range files {
		data, err := examples.FS.ReadFile(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "init: embedded %s: %v\n", src, err)
			return 1
		}
		if src == "default-pipeline.yaml" {
			s := strings.ReplaceAll(string(data), examples.PlaceholderRepo, *repo)
			s = strings.ReplaceAll(s, examples.DefaultLabel, *label)
			data = []byte(s)
		}
		path := filepath.Join(*dir, dst)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "init: %v\n", err)
			return 1
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "init: %v\n", err)
			return 1
		}
	}

	cfg := filepath.Join(*dir, "pipeline.yaml")
	fmt.Printf(`scaffolded %s + %s/prompts/ for %s

next steps:
  orchestratord validate %s
  gh label create %s --repo %s   # the label the daemon polls
  orchestratord doctor --config %s --repo <local-checkout>
      # preflights everything the daemon needs, including a real
      # kickoff-delivery smoke test. Run it until it is green, then:
  then follow RUNBOOK.md §3 (bring-up)
`, cfg, *dir, *repo, cfg, *label, *repo, cfg)
	return 0
}
