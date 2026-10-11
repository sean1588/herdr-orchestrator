package examples

import (
	"os"
	"path/filepath"
	"slices"
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

// The launch line each verified agent runs under, as README.md "Which agent"
// and the comment above roles: in default-pipeline.yaml give it.
func TestEmbeddedExampleValidatesWithEachAgentLaunch(t *testing.T) {
	data, err := FS.ReadFile("default-pipeline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const shipped = `launch: ["claude"]`
	tests := []struct {
		agent  string
		launch string
		want   []string
	}{
		{"claude", `["claude"]`, []string{"claude"}},
		{"codex", `["codex", "-a", "never", "--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true"]`,
			[]string{"codex", "-a", "never", "--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true"}},
		{"grok", `["grok", "--always-approve"]`, []string{"grok", "--always-approve"}},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			line := "launch: " + tt.launch
			documented := false
			for _, l := range strings.Split(string(data), "\n") {
				l = strings.TrimSpace(l)
				documented = documented || (strings.HasPrefix(l, "#") && strings.HasSuffix(l, line))
			}
			if !documented {
				t.Fatalf("default-pipeline.yaml has no comment carrying %q", line)
			}
			path := writeFS(t, t.TempDir())
			substituted := strings.ReplaceAll(string(data), shipped, line)
			if err := os.WriteFile(path, []byte(substituted), 0o644); err != nil {
				t.Fatal(err)
			}
			wf, _, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if errs := engine.CheckExecutable(wf); len(errs) > 0 {
				t.Fatalf("CheckExecutable: %v", errs)
			}
			if len(wf.Roles) == 0 {
				t.Fatal("no roles")
			}
			for name, role := range wf.Roles {
				if !slices.Equal(role.Launch, tt.want) {
					t.Errorf("role %s launch = %q, want %q", name, role.Launch, tt.want)
				}
			}
		})
	}
}
