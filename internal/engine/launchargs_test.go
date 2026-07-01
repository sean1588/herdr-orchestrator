package engine

import (
	"reflect"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
)

func TestLaunchArgs_AppendsAllowedToolsForClaude(t *testing.T) {
	got := launchArgs(config.Role{Launch: []string{"claude"}, AllowedTools: []string{"Read", "Bash(gh pr view:*)"}})
	want := []string{"claude", "--allowedTools", "Read,Bash(gh pr view:*)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("launchArgs = %v, want %v", got, want)
	}
	// No allowlist => unchanged launch.
	if g := launchArgs(config.Role{Launch: []string{"claude"}}); !reflect.DeepEqual(g, []string{"claude"}) {
		t.Errorf("unscoped launch changed: %v", g)
	}
	// Non-claude launcher => flag not applied (translation is claude-targeted).
	if g := launchArgs(config.Role{Launch: []string{"aider"}, AllowedTools: []string{"Read"}}); !reflect.DeepEqual(g, []string{"aider"}) {
		t.Errorf("non-claude launcher scoped: %v", g)
	}
}
