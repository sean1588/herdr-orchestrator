package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

func TestFindPR(t *testing.T) {
	tests := []struct {
		name     string
		stdout   string
		runErr   error
		wantPR   *PR
		wantErr  bool
		wantArgv []string
	}{
		{
			name:     "non-empty array returns first PR",
			stdout:   `[{"number":12,"url":"https://github.com/o/r/pull/12","state":"OPEN"}]`,
			wantPR:   &PR{Number: 12, URL: "https://github.com/o/r/pull/12", State: "OPEN"},
			wantArgv: []string{"pr", "list", "--head", "agent/issue-5", "--json", "number,url,state"},
		},
		{
			name:     "empty array returns nil, nil",
			stdout:   `[]`,
			wantPR:   nil,
			wantArgv: []string{"pr", "list", "--head", "agent/issue-5", "--json", "number,url,state"},
		},
		{
			name:     "runner error is propagated",
			runErr:   errors.New("gh exploded"),
			wantErr:  true,
			wantArgv: []string{"pr", "list", "--head", "agent/issue-5", "--json", "number,url,state"},
		},
		{
			name:     "unparseable json is an error",
			stdout:   `not json`,
			wantErr:  true,
			wantArgv: []string{"pr", "list", "--head", "agent/issue-5", "--json", "number,url,state"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
				return []byte(tt.stdout), tt.runErr
			}}
			c := New(fake)

			got, err := c.FindPR(context.Background(), "/repo", "agent/issue-5")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("FindPR: want error, got nil (pr=%+v)", got)
				}
			} else if err != nil {
				t.Fatalf("FindPR: unexpected error: %v", err)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.wantPR) {
				t.Errorf("FindPR pr = %+v, want %+v", got, tt.wantPR)
			}

			if len(fake.Calls) != 1 {
				t.Fatalf("want 1 call, got %d", len(fake.Calls))
			}
			call := fake.Calls[0]
			if call.Name != "gh" {
				t.Errorf("Name = %q, want gh", call.Name)
			}
			if call.Dir != "/repo" {
				t.Errorf("Dir = %q, want /repo", call.Dir)
			}
			if !reflect.DeepEqual(call.Args, tt.wantArgv) {
				t.Errorf("Args = %v, want %v", call.Args, tt.wantArgv)
			}
		})
	}
}

func TestFindPR_RunnerErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("boom")
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		return nil, sentinel
	}}
	c := New(fake)

	_, err := c.FindPR(context.Background(), "/repo", "agent/issue-5")
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap sentinel", err)
	}
}

func TestIssue(t *testing.T) {
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		return []byte(`{"number":7,"title":"Fix bug","body":"do the thing"}`), nil
	}}
	c := New(fake)

	got, err := c.Issue(context.Background(), "/repo", 7)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	want := &Issue{Number: 7, Title: "Fix bug", Body: "do the thing"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Issue = %+v, want %+v", got, want)
	}

	if len(fake.Calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(fake.Calls))
	}
	call := fake.Calls[0]
	if call.Name != "gh" {
		t.Errorf("Name = %q, want gh", call.Name)
	}
	if call.Dir != "/repo" {
		t.Errorf("Dir = %q, want /repo", call.Dir)
	}
	wantArgv := []string{"issue", "view", "7", "--json", "number,title,body"}
	if !reflect.DeepEqual(call.Args, wantArgv) {
		t.Errorf("Args = %v, want %v", call.Args, wantArgv)
	}
}

func TestIssue_RunnerErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("no such issue")
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		return nil, sentinel
	}}
	c := New(fake)

	_, err := c.Issue(context.Background(), "/repo", 99)
	if err == nil {
		t.Fatal("want error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error %v does not wrap sentinel", err)
	}
}

func TestIssue_HonorsRepoDir(t *testing.T) {
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		return []byte(`{"number":1,"title":"t","body":"b"}`), nil
	}}
	c := New(fake)

	if _, err := c.Issue(context.Background(), "/other/checkout", 1); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got := fake.Calls[0].Dir; got != "/other/checkout" {
		t.Errorf("Dir = %q, want /other/checkout", got)
	}
}

func TestIssue_UnparseableJSONIsError(t *testing.T) {
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
		return []byte(`{broken`), nil
	}}
	c := New(fake)

	_, err := c.Issue(context.Background(), "/repo", 7)
	if err == nil || !strings.Contains(err.Error(), "issue") {
		t.Errorf("want wrapped parse error mentioning issue, got %v", err)
	}
}
