package engine

import (
	"reflect"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/config"
)

func TestLaunchArgs_AppendsAllowedToolsForClaude(t *testing.T) {
	// Tools are appended as separate args (claude's --allowedTools is variadic
	// `<tools...>`), not comma-joined into one token.
	got := launchArgs(config.Role{Launch: []string{"claude"}, AllowedTools: []string{"Read", "Edit"}})
	want := []string{"claude", "--allowedTools", "Read", "Edit"}
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
	// An absolute-path claude launcher is still recognized (match by basename).
	gotAbs := launchArgs(config.Role{Launch: []string{"/usr/local/bin/claude"}, AllowedTools: []string{"Read"}})
	wantAbs := []string{"/usr/local/bin/claude", "--allowedTools", "Read"}
	if !reflect.DeepEqual(gotAbs, wantAbs) {
		t.Errorf("absolute-path claude launcher = %v, want %v", gotAbs, wantAbs)
	}
}
