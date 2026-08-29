package config

import (
	"strings"
	"testing"
	"time"
)

func TestResolveDriveDeadline(t *testing.T) {
	withTimeouts := func(ds ...string) map[string]State {
		states := map[string]State{}
		for i, d := range ds {
			states[string(rune('a'+i))] = State{Transitions: []Transition{
				{When: Trigger{Timeout: d}, To: "escalated"},
			}}
		}
		return states
	}

	tests := []struct {
		name     string
		policies Policies
		states   map[string]State
		want     time.Duration
	}{
		{
			name:     "explicit value wins",
			policies: Policies{DriveDeadline: "20m"},
			states:   withTimeouts("90m"),
			want:     20 * time.Minute,
		},
		{
			name:   "derived from twice the longest state timeout",
			states: withTimeouts("15m", "90m", "30m"),
			want:   3 * time.Hour,
		},
		{
			name:   "floored at an hour when the timeouts are short",
			states: withTimeouts("5m", "10m"),
			want:   time.Hour,
		},
		{
			name:   "floored at an hour when no state declares a timeout",
			states: map[string]State{"a": {}},
			want:   time.Hour,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wf := &Workflow{Policies: tc.policies, States: tc.states}
			got, err := wf.ResolveDriveDeadline()
			if err != nil {
				t.Fatalf("ResolveDriveDeadline: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveDriveDeadline() = %s, want %s", got, tc.want)
			}
		})
	}
}

// A ceiling at or below a declared state timeout is not a backstop but a
// competing deadline: every task in that state would be reaped before its own
// timeout transition could fire. Reject it rather than let it silently preempt.
func TestCheckDriveDeadlineRejectsACeilingBelowAStateTimeout(t *testing.T) {
	states := map[string]State{"implementing": {Transitions: []Transition{
		{When: Trigger{Timeout: "90m"}, To: "escalated"},
	}}}

	tests := []struct {
		name     string
		deadline string
		wantErr  bool
	}{
		{"below the longest state timeout", "30m", true},
		{"equal to it", "90m", true},
		{"safely above it", "3h", false},
		{"absent, so derived", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var errs []string
			checkDriveDeadline(&Workflow{Policies: Policies{DriveDeadline: tc.deadline}, States: states}, &errs)
			if got := len(errs) > 0; got != tc.wantErr {
				t.Fatalf("errors = %v, want error: %v", errs, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(errs[0], "longest state timeout") {
				t.Errorf("error should name the conflict, got: %s", errs[0])
			}
		})
	}
}

func TestResolveNoProgressTimeout(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"absent applies the global default", "", defaultNoProgressTimeout, false},
		{"explicit value wins", "45m", 45 * time.Minute, false},
		{"zero disables the bound", "0s", 0, false},
		{"malformed is rejected", "banana", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Policies{NoProgressTimeout: tc.value}.ResolveNoProgressTimeout()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("ResolveNoProgressTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}
