package github

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

func TestMerge_Argv(t *testing.T) {
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return nil, nil }}
	if err := New(fake).Merge(context.Background(), "/repo", 12); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(fake.Calls))
	}
	call := fake.Calls[0]
	wantArgv := []string{"pr", "merge", "12", "--squash", "--delete-branch"}
	if call.Name != "gh" || call.Dir != "/repo" || !reflect.DeepEqual(call.Args, wantArgv) {
		t.Errorf("call = {%q %q %v}, want {gh /repo %v}", call.Name, call.Dir, call.Args, wantArgv)
	}
}

func TestMerge_RunnerErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("not mergeable")
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return nil, sentinel }}
	if err := New(fake).Merge(context.Background(), "/repo", 12); err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel, got %v", err)
	}
}
