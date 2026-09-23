package classify

import (
	"context"
	"regexp"
	"strings"
)

// Heuristic recognises Claude Code's permission prompts by their rendered
// structure. It needs no network, no key, and no dependencies, which is its
// whole reason to exist: a keyless setup still gets the cheapest, most common
// wedge (#37, an agent parked on a permission prompt) escalated at once instead
// of after the full no-progress window.
//
// It answers exactly one label. A tail whose final box matches a known prompt
// shape is AwaitingPermission at confidence 1; anything else is the zero Result,
// which is below Threshold, so the engine does exactly what it does with no
// classifier at all.
//
// Do not "improve" it into answering Working, Finished, Crashed, or
// AwaitingAnswer. A pattern cannot know those: it cannot tell a silent test run
// from a dead pane, or a finished agent from one about to continue. Guessing
// there would turn a missed classification — harmless, since it falls back to
// today's no_progress escalation — into a wrong one. The false-escalation case
// is Jev's to fix, not this type's.
//
// No match is the zero Result, never an error: an error means "I failed", the
// zero Result means "I have nothing to add", and a pane that simply is not a
// prompt must not log as a failure.
//
// Claude Code only. Other agents render their prompts differently, which is a
// reason this stays opt-in, not a reason to grow a shape per agent.
type Heuristic struct{}

// Classify reports AwaitingPermission when the tail's final box is a Claude
// Code permission or trust prompt, and the zero Result otherwise.
func (Heuristic) Classify(_ context.Context, paneTail string) (Result, error) {
	box := finalBox(paneTail)
	for _, s := range promptShapes {
		if s.matches(box) {
			return Result{Activity: AwaitingPermission, Confidence: 1}, nil
		}
	}
	return Result{}, nil
}

// shape is a prompt's anchors: line patterns that must match, in order, within
// one box. Each is matched against a line with its indentation trimmed.
type shape []*regexp.Regexp

func (s shape) matches(box []string) bool {
	next := 0
	for _, line := range box {
		if next < len(s) && s[next].MatchString(strings.TrimSpace(line)) {
			next++
		}
	}
	return next == len(s)
}

// finalBox returns the lines below the pane's last rule: a line drawn from
// column 0 entirely in '─'. Claude Code draws that rule across the full width
// above whatever owns the bottom of the screen — a prompt box, or the input box
// of a working agent. Anchoring to it is what keeps prompt-shaped text elsewhere
// in the pane (tool output, an answered prompt still in the transcript) from
// matching: only the box that is live now is read. No rule, no box.
func finalBox(tail string) []string {
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if isRule(lines[i]) {
			return lines[i+1:]
		}
	}
	return nil
}

func isRule(line string) bool {
	line = strings.TrimRight(line, " \t\r")
	return line != "" && strings.Trim(line, "─") == ""
}

// promptShapes are the prompts Heuristic recognises. Each shape records the
// rendered block it was derived from and the Claude Code version, so that when
// the rendering changes a fixture changes with it and the tests fail loudly
// rather than the match going silently stale. The fixtures under
// testdata/panes/awaiting_permission-* are those renders.
var promptShapes = []shape{
	// The tool-permission box (Bash, Write, Edit, WebFetch, ...). Claude Code
	// v2.1.280, captured live:
	//
	//	────────────────────────────────────────────────────────────
	//	 Bash command
	//	 Tip: auto mode handles these prompts for you — choose "switch to auto mode" below
	//
	//	   mkdir -p build && echo Yes proceed > build/out.txt
	//	   Create build dir and write output file
	//
	//	 Do you want to proceed?
	//	 ❯ 1. Yes
	//	   2. Yes, and don't ask again for mkdir -p build and echo Yes proceed commands in
	//	      /Users/dev/projects/ledger-api
	//	   3. Yes, and switch to auto mode · auto mode handles these prompts for you
	//	   4. No
	//
	//	 Esc to cancel · Tab to amend
	//
	// The question varies by tool ("Do you want to create notes.md?", "Do you
	// want to make this edit to existing.md?", "Do you want to allow Claude to
	// fetch this content?"), and so does the No option ("3. No, and tell Claude
	// what to do differently (esc)" on WebFetch). The footer is not an anchor:
	// WebFetch renders none. The ❯ cursor sits on whichever option is selected.
	{
		regexp.MustCompile(`^Do you want to .+\?$`),
		regexp.MustCompile(`^(?:❯ )?1\. Yes$`),
		regexp.MustCompile(`^(?:❯ )?\d+\. No\b`),
	},
	// The trust prompt on a folder with no trusted ancestor, which is what a
	// brand-new worktree outside a trusted tree gets. Claude Code v2.1.280,
	// captured live:
	//
	//	────────────────────────────────────────────────────────────
	//	 Accessing workspace:
	//
	//	 /Users/dev/projects/ledger-api
	//
	//	 Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source project, or work from your team). If not, take a moment to review what's in this folder
	//	 first.
	//
	//	 Claude Code'll be able to read, edit, and execute files here.
	//
	//	 Security guide
	//
	//	 ❯ No, exit
	//	   Yes, I trust this folder
	//
	//	 Enter to confirm · Esc to cancel
	{
		regexp.MustCompile(`^Accessing workspace:$`),
		regexp.MustCompile(`^(?:❯ )?No, exit$`),
		regexp.MustCompile(`^(?:❯ )?Yes, I trust this folder$`),
	},
}
