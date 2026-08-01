package examples

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/engine"
)

// writeFS materializes the embedded example into dir and returns the config path.
func writeFS(t *testing.T, dir string) string {
	t.Helper()
	for _, name := range []string{"default-pipeline.yaml", "prompts/triage.md", "prompts/review.md"} {
		data, err := FS.ReadFile(name)
		if err != nil {
			t.Fatalf("embedded file %s: %v", name, err)
		}
		dst := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "default-pipeline.yaml")
}

func TestEmbeddedExampleIsValidAndExecutable(t *testing.T) {
	path := writeFS(t, t.TempDir())
	wf, warnings, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, w := range warnings {
		t.Logf("warning: %s", w)
	}
	if errs := engine.CheckExecutable(wf); len(errs) > 0 {
		t.Fatalf("CheckExecutable: %v", errs)
	}
}

func TestEmbeddedExampleContainsPlaceholders(t *testing.T) {
	data, err := FS.ReadFile("default-pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{PlaceholderRepo, DefaultLabel} {
		if !strings.Contains(string(data), tok) {
			t.Errorf("default-pipeline.yaml missing token %q that init substitutes", tok)
		}
	}
}

func TestEmbeddedExampleDefaultGateOmitsApprovals(t *testing.T) {
	path := writeFS(t, t.TempDir())
	wf, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wf.Gates["approvals"]; !ok {
		t.Fatal("approvals gate should stay defined for team repos to opt into")
	}
	for _, state := range []string{"approved", "blocked_on_gate"} {
		for _, tr := range wf.States[state].Transitions {
			for _, g := range tr.GateRefs() {
				if g == "approvals" {
					t.Errorf("state %s references approvals; default must merge without it (issue #36)", state)
				}
			}
		}
	}
}
