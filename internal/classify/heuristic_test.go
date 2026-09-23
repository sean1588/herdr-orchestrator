package classify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both halves of the corpus: every awaiting_permission fixture is recognised at
// confidence 1, and every other fixture — including a working pane whose tool
// output holds a whole permission-shaped block — gets the zero Result. The
// negative half is the one that cannot pass vacuously: a pattern that matched
// "Yes" anywhere would pass every positive fixture and fail here.
func TestHeuristic_FixtureCorpus(t *testing.T) {
	paths, err := filepath.Glob("testdata/panes/*.txt")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	seen := map[Activity]int{}
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".txt")
		label := Activity(name[:strings.LastIndex(name, "-")])
		seen[label]++
		t.Run(name, func(t *testing.T) {
			tail, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Heuristic{}.Classify(context.Background(), string(tail))
			if err != nil {
				t.Fatalf("err = %v; no match must be the zero Result, never an error", err)
			}
			want := Result{}
			if label == AwaitingPermission {
				want = Result{Activity: AwaitingPermission, Confidence: 1}
			}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
	for a := range criteria {
		if seen[a] == 0 {
			t.Errorf("no %s fixtures: that half of the corpus would pass vacuously", a)
		}
	}
}

// Structure, not words: the anchors only count inside the box below the last
// rule, and all of them must be there.
func TestHeuristic_AnchorsToTheFinalBox(t *testing.T) {
	const rule = "────────────────────────────────────────"
	tests := []struct {
		name string
		tail string
		want Result
	}{
		{
			name: "whole prompt in the final box",
			tail: "⏺ Bash(make)\n" + rule + "\n Bash command\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n",
			want: Result{Activity: AwaitingPermission, Confidence: 1},
		},
		{
			name: "cursor moved off the first option",
			tail: rule + "\n Do you want to proceed?\n   1. Yes\n ❯ 2. No\n",
			want: Result{Activity: AwaitingPermission, Confidence: 1},
		},
		{
			name: "prompt above the input box is history, not the live box",
			tail: rule + "\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n" + rule + "\n❯\n" + rule + "\n  ⏸ manual mode on\n",
			want: Result{},
		},
		{
			name: "no rule, no box",
			tail: " Do you want to proceed?\n ❯ 1. Yes\n   2. No\n",
			want: Result{},
		},
		{
			name: "a rule indented in tool output does not open a box",
			tail: "  ⎿  Running…\n     " + rule + "\n     Do you want to proceed?\n     ❯ 1. Yes\n       2. No\n",
			want: Result{},
		},
		{
			name: "no No option",
			tail: rule + "\n Do you want to proceed?\n ❯ 1. Yes\n   2. Yes, and don't ask again\n",
			want: Result{},
		},
		{
			name: "anchors out of order",
			tail: rule + "\n ❯ 1. Yes\n   2. No\n Do you want to proceed?\n",
			want: Result{},
		},
		{
			name: "empty tail",
			tail: "",
			want: Result{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Heuristic{}.Classify(context.Background(), tt.tail)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The nearest miss in practice: AskUserQuestion (Claude Code v2.1.280, captured
// live) renders an agent-written question and a numbered "1. Yes" / "2. No"
// list, every anchor of the permission box. What tells it apart is the rule it
// draws below its options, which leaves the question outside the final box.
//
// It lives outside testdata/panes because Jev reads this render as
// awaiting_permission at p=0.81 — below Threshold, so harmless to the engine,
// but it would fail the live corpus test that asserts its true label.
func TestHeuristic_AskUserQuestionIsNotAPermissionPrompt(t *testing.T) {
	tail, err := os.ReadFile("testdata/askuserquestion.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Heuristic{}.Classify(context.Background(), string(tail))
	if err != nil || got != (Result{}) {
		t.Fatalf("got (%+v, %v), want the zero Result", got, err)
	}
}
