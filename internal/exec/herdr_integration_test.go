//go:build !windows

package exec_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/exec"
	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

// writeExec writes an executable script to path (0755).
func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fixtureAbs resolves a testdata path to absolute (for embedding in the fake).
func fixtureAbs(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// TestHerdr_Resolve_AgainstFakeBinary runs the REAL exec.Herdr against a fake
// `herdr` on PATH that emits captured `workspace list` / `pane list` JSON. It
// pins the CLI JSON contract our in-process fakes assume: if herdr's output
// shape drifts, this fails where the unit tests (which mock proc) would not.
func TestHerdr_Resolve_AgainstFakeBinary(t *testing.T) {
	ws := fixtureAbs(t, "testdata/workspace_list.json")
	panes := fixtureAbs(t, "testdata/pane_list.json")
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = workspace ] && [ \"$2\" = list ]; then cat " + ws + "; exit 0; fi\n" +
		"if [ \"$1\" = pane ] && [ \"$2\" = list ]; then cat " + panes + "; exit 0; fi\n" +
		"exit 9\n"
	writeExec(t, filepath.Join(dir, "herdr"), body)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	h := exec.NewHerdr(proc.New()) // HerdrBin defaults to "herdr" (PATH lookup)

	hnd, ok, err := h.Resolve(context.Background(), "issue-208")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true: workspace issue-208 is in the fixture")
	}
	if hnd.PaneID != "wA:p1" {
		t.Errorf("PaneID = %q, want wA:p1", hnd.PaneID)
	}
	if hnd.Workdir != "/home/sean/github/wt-issue-208" {
		t.Errorf("Workdir = %q, want the pane's cwd", hnd.Workdir)
	}

	// A label absent from the fixture resolves to not-found, no error.
	if _, ok, err := h.Resolve(context.Background(), "issue-999"); err != nil || ok {
		t.Errorf("Resolve(absent) = ok:%v err:%v, want ok:false err:nil", ok, err)
	}
}

// TestHerdr_Message_AgentBlockedAgainstFakeBinary pins the error contract the
// delivery path depends on: herdr writes its {"error":{"code":...}} envelope to
// stderr and exits non-zero, and the code must survive the real proc.Runner to
// be decoded. A blocked prompt must come back naming the explain rule.
func TestHerdr_Message_AgentBlockedAgainstFakeBinary(t *testing.T) {
	explain := fixtureAbs(t, "testdata/agent_explain_blocked.json")
	dir := t.TempDir()
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = prompt ]; then " +
		"echo '{\"error\":{\"code\":\"agent_blocked\",\"message\":\"synthetic\"},\"id\":\"cli:agent:prompt\"}' >&2; exit 1; fi\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = explain ]; then cat " + explain + "; exit 0; fi\n" +
		"exit 9\n"
	writeExec(t, filepath.Join(dir, "herdr"), body)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := exec.NewHerdr(proc.New()).Message(context.Background(), exec.Handle{PaneID: "wB:p1"}, "hello")
	if err == nil {
		t.Fatal("Message succeeded against an agent herdr reports blocked")
	}
	if want := "agent blocked before kickoff on wB:p1: trust_directory"; err.Error() != want {
		t.Errorf("Message error = %q, want %q", err, want)
	}
}
