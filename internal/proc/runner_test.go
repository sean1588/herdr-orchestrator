package proc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunner_CapturesStdout(t *testing.T) {
	out, err := New().Run(context.Background(), "", "echo", "hello world")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello world" {
		t.Errorf("stdout = %q", out)
	}
}

func TestRunner_HonorsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := New().Run(context.Background(), dir, "ls")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(out), "marker.txt") {
		t.Errorf("ls in %s did not list marker.txt: %q", dir, out)
	}
}

func TestRunner_ErrorWrapsStderr(t *testing.T) {
	out, err := New().Run(context.Background(), "", "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected error for exit 3")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should include stderr, got: %v", err)
	}
	_ = out
}

func TestNewScrubbed_RemovesNamedEnv(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "pat-secret")
	// A plain runner inherits the parent environment.
	out, err := New().Run(context.Background(), "", "sh", "-c", `printf %s "$GITHUB_TOKEN"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "pat-secret" {
		t.Fatalf("plain runner should inherit GITHUB_TOKEN, got %q", out)
	}
	// A scrubbed runner drops it, so `gh` falls back to its stored OAuth token.
	out, err = NewScrubbed("GITHUB_TOKEN").Run(context.Background(), "", "sh", "-c", `printf %s "$GITHUB_TOKEN"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("scrubbed runner should drop GITHUB_TOKEN, got %q", out)
	}
}

func TestFake_RecordsCallsAndScriptsResponses(t *testing.T) {
	f := &Fake{Responder: func(c Call) ([]byte, error) {
		if c.Name == "herdr" && len(c.Args) > 0 && c.Args[0] == "workspace" {
			return []byte(`{"ok":true}`), nil
		}
		return nil, nil
	}}
	out, err := f.Run(context.Background(), "/repo", "herdr", "workspace", "create")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if string(out) != `{"ok":true}` {
		t.Errorf("scripted output = %q", out)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want 1 recorded call, got %d", len(f.Calls))
	}
	c := f.Calls[0]
	if c.Dir != "/repo" || c.Name != "herdr" || strings.Join(c.Args, " ") != "workspace create" {
		t.Errorf("recorded call = %+v", c)
	}
}
