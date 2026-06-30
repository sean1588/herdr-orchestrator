package github

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sean1588/herdr-orchestrator/internal/proc"
)

func TestPRStatus(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		want   PRStatus
	}{
		{
			name: "clean PR: checks pass, one approval, mergeable",
			stdout: `{
			  "state":"OPEN","reviewDecision":"APPROVED","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",
			  "statusCheckRollup":[
			    {"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},
			    {"__typename":"StatusContext","state":"SUCCESS"}
			  ],
			  "reviews":[{"author":{"login":"alice"},"state":"APPROVED","submittedAt":"2026-06-01T00:00:00Z"}]
			}`,
			want: PRStatus{State: "OPEN", ChecksTotal: 2, ChecksFailed: 0, ChecksPending: 0,
				ApprovedReviews: 1, ReviewDecision: "APPROVED", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"},
		},
		{
			name: "a failing check",
			stdout: `{"state":"OPEN","statusCheckRollup":[
			  {"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"},
			  {"__typename":"CheckRun","status":"COMPLETED","conclusion":"FAILURE"}
			],"reviews":[]}`,
			want: PRStatus{State: "OPEN", ChecksTotal: 2, ChecksFailed: 1, ChecksPending: 0},
		},
		{
			name: "a pending check",
			stdout: `{"state":"OPEN","statusCheckRollup":[
			  {"__typename":"CheckRun","status":"IN_PROGRESS","conclusion":""},
			  {"__typename":"StatusContext","state":"PENDING"}
			],"reviews":[]}`,
			want: PRStatus{State: "OPEN", ChecksTotal: 2, ChecksFailed: 0, ChecksPending: 2},
		},
		{
			name:   "no checks is vacuously green",
			stdout: `{"state":"OPEN","statusCheckRollup":[],"reviews":[]}`,
			want:   PRStatus{State: "OPEN", ChecksTotal: 0},
		},
		{
			name: "latest review per author wins: approve then request_changes => 0",
			stdout: `{"state":"OPEN","reviews":[
			  {"author":{"login":"alice"},"state":"APPROVED","submittedAt":"2026-06-01T00:00:00Z"},
			  {"author":{"login":"alice"},"state":"CHANGES_REQUESTED","submittedAt":"2026-06-02T00:00:00Z"}
			]}`,
			want: PRStatus{State: "OPEN", ApprovedReviews: 0},
		},
		{
			name: "latest review per author wins: comment then approve => 1",
			stdout: `{"state":"OPEN","reviews":[
			  {"author":{"login":"bob"},"state":"COMMENTED","submittedAt":"2026-06-01T00:00:00Z"},
			  {"author":{"login":"bob"},"state":"APPROVED","submittedAt":"2026-06-03T00:00:00Z"}
			]}`,
			want: PRStatus{State: "OPEN", ApprovedReviews: 1},
		},
		{
			name: "two distinct approvers => 2",
			stdout: `{"state":"OPEN","reviews":[
			  {"author":{"login":"alice"},"state":"APPROVED","submittedAt":"2026-06-01T00:00:00Z"},
			  {"author":{"login":"carol"},"state":"APPROVED","submittedAt":"2026-06-01T00:00:00Z"}
			]}`,
			want: PRStatus{State: "OPEN", ApprovedReviews: 2},
		},
		{
			name:   "merged state is reported",
			stdout: `{"state":"MERGED","statusCheckRollup":[],"reviews":[]}`,
			want:   PRStatus{State: "MERGED"},
		},
	}

	wantArgv := []string{"pr", "view", "42", "--json", prStatusFields}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) {
				return []byte(tt.stdout), nil
			}}
			c := New(fake)

			got, err := c.PRStatus(context.Background(), "/repo", 42)
			if err != nil {
				t.Fatalf("PRStatus: %v", err)
			}
			if *got != tt.want {
				t.Errorf("PRStatus = %+v\n          want %+v", *got, tt.want)
			}
			if got.ChecksGreen() != (tt.want.ChecksFailed == 0 && tt.want.ChecksPending == 0) {
				t.Errorf("ChecksGreen() = %v", got.ChecksGreen())
			}
			if len(fake.Calls) != 1 {
				t.Fatalf("want 1 call, got %d", len(fake.Calls))
			}
			if call := fake.Calls[0]; call.Name != "gh" || call.Dir != "/repo" || !reflect.DeepEqual(call.Args, wantArgv) {
				t.Errorf("call = {%q %q %v}, want {gh /repo %v}", call.Name, call.Dir, call.Args, wantArgv)
			}
		})
	}
}

func TestPRStatus_RunnerErrorIsWrapped(t *testing.T) {
	sentinel := errors.New("gh down")
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return nil, sentinel }}
	if _, err := New(fake).PRStatus(context.Background(), "/repo", 1); err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel, got %v", err)
	}
}

func TestPRStatus_UnparseableJSONIsError(t *testing.T) {
	fake := &proc.Fake{Responder: func(c proc.Call) ([]byte, error) { return []byte(`{broken`), nil }}
	if _, err := New(fake).PRStatus(context.Background(), "/repo", 1); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("want parse error, got %v", err)
	}
}
