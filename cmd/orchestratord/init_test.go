package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/engine"
)

func TestCmdInit_ScaffoldsValidConfig(t *testing.T) {
	dir := t.TempDir()
	if got := run([]string{"init", "--repo", "alice/widgets", "--dir", dir}); got != 0 {
		t.Fatalf("init exit = %d, want 0", got)
	}

	cfgPath := filepath.Join(dir, "pipeline.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("pipeline.yaml not written: %v", err)
	}
	if strings.Contains(string(data), "your-github-user/your-repo") {
		t.Error("placeholder repo survived substitution")
	}
	if !strings.Contains(string(data), "repo: alice/widgets") {
		t.Error("target repo not substituted in")
	}
	for _, p := range []string{"prompts/triage.md", "prompts/review.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s not written: %v", p, err)
		}
	}

	wf, _, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("scaffolded config invalid: %v", err)
	}
	if errs := engine.CheckExecutable(wf); len(errs) > 0 {
		t.Fatalf("scaffolded config not executable: %v", errs)
	}
	if wf.Sources[0].Repo != "alice/widgets" {
		t.Errorf("source repo = %q, want alice/widgets", wf.Sources[0].Repo)
	}
}

func TestCmdInit_SubstitutesLabel(t *testing.T) {
	dir := t.TempDir()
	if got := run([]string{"init", "--repo", "alice/widgets", "--label", "bot-ready", "--dir", dir}); got != 0 {
		t.Fatalf("init exit = %d, want 0", got)
	}
	wf, _, err := config.Load(filepath.Join(dir, "pipeline.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := wf.Sources[0].Select["label"]; got != "bot-ready" {
		t.Errorf("source label = %v, want bot-ready", got)
	}
}

func TestCmdInit_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	// Only one of the three targets exists — init must still refuse and
	// write nothing (no partial scaffold).
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "prompts", "triage.md")
	if err := os.WriteFile(existing, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := run([]string{"init", "--repo", "alice/widgets", "--dir", dir}); got != 1 {
		t.Fatalf("init exit = %d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "pipeline.yaml")); !os.IsNotExist(err) {
		t.Error("pipeline.yaml written despite refusal — partial scaffold")
	}
	if data, _ := os.ReadFile(existing); string(data) != "mine" {
		t.Error("existing file clobbered")
	}
}

func TestCmdInit_RejectsBadRepo(t *testing.T) {
	for _, repo := range []string{"", "noslash", "a/b/c", "/name", "owner/"} {
		if got := run([]string{"init", "--repo", repo, "--dir", t.TempDir()}); got != 2 {
			t.Errorf("init --repo %q exit = %d, want 2", repo, got)
		}
	}
}

func TestCmdInit_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "orchestrator")
	if got := run([]string{"init", "--repo", "alice/widgets", "--dir", dir}); got != 0 {
		t.Fatalf("init exit = %d, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "pipeline.yaml")); err != nil {
		t.Errorf("pipeline.yaml not written in created dir: %v", err)
	}
}
