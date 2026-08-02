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
	// No --delete-branch: gh's local-branch delete cannot succeed (the branch is
	// checked out in the task's worktree), and its non-zero exit would report an
	// already-merged PR as a merge failure. Branch teardown is DeleteRemoteBranch.
	wantArgv := []string{"pr", "merge", "12", "--squash"}
	if call.Name != "gh" || call.Dir != "/repo" || !reflect.DeepEqual(call.Args, wantArgv) {
		t.Errorf("call = {%q %q %v}, want {gh /repo %v}", call.Name, call.Dir, call.Args, wantArgv)
	}
}

func TestDeleteRemoteBranch_Argv(t *testing.T) {
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return nil, nil }}
	if err := New(fake).DeleteRemoteBranch(context.Background(), "/repo", "agent/issue-5"); err != nil {
		t.Fatalf("DeleteRemoteBranch: %v", err)
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(fake.Calls))
	}
	call := fake.Calls[0]
	wantArgv := []string{"api", "--method", "DELETE", "repos/{owner}/{repo}/git/refs/heads/agent/issue-5"}
	if call.Name != "gh" || call.Dir != "/repo" || !reflect.DeepEqual(call.Args, wantArgv) {
		t.Errorf("call = {%q %q %v}, want {gh /repo %v}", call.Name, call.Dir, call.Args, wantArgv)
	}
}

func TestDeleteRemoteBranch_RunnerErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("branch not found")
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return nil, sentinel }}
	err := New(fake).DeleteRemoteBranch(context.Background(), "/repo", "agent/issue-5")
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel, got %v", err)
	}
}

func TestCloseIssue_Argv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		comment string
		want    []string
	}{
		{
			name:    "with comment",
			comment: "Merged in #12.",
			want:    []string{"issue", "close", "5", "--reason", "completed", "--comment", "Merged in #12."},
		},
		{
			// An empty comment must not become an empty --comment flag, which gh
			// would post as a blank comment.
			name: "without comment",
			want: []string{"issue", "close", "5", "--reason", "completed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return nil, nil }}
			if err := New(fake).CloseIssue(context.Background(), "/repo", 5, tc.comment); err != nil {
				t.Fatalf("CloseIssue: %v", err)
			}
			if len(fake.Calls) != 1 {
				t.Fatalf("want 1 call, got %d", len(fake.Calls))
			}
			call := fake.Calls[0]
			if call.Name != "gh" || call.Dir != "/repo" || !reflect.DeepEqual(call.Args, tc.want) {
				t.Errorf("call = {%q %q %v}, want {gh /repo %v}", call.Name, call.Dir, call.Args, tc.want)
			}
		})
	}
}

func TestCloseIssue_RunnerErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("no such issue")
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return nil, sentinel }}
	err := New(fake).CloseIssue(context.Background(), "/repo", 5, "")
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel, got %v", err)
	}
}

func TestMerge_RunnerErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("not mergeable")
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return nil, sentinel }}
	if err := New(fake).Merge(context.Background(), "/repo", 12); err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel, got %v", err)
	}
}
