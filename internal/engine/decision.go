package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sean1588/herdr-orchestrator/internal/config"
	"github.com/sean1588/herdr-orchestrator/internal/store"
)

// verdict is what a reviewer agent writes for an `llm` decision: one of the
// decision's declared verdicts plus optional feedback the engine forwards to a
// resumed implementer on request_changes.
type verdict struct {
	Verdict  string `json:"verdict"`
	Feedback string `json:"feedback"`
}

// verdictPath is where a reviewer writes (and the engine reads) the verdict for a
// task's decision. One per task; a later review round overwrites the prior one.
func verdictPath(taskDir, taskID string) string {
	return filepath.Join(taskDir, "verdict-"+taskID+".json")
}

func readVerdict(taskDir, taskID string) (*verdict, error) {
	b, err := os.ReadFile(verdictPath(taskDir, taskID))
	if err != nil {
		return nil, fmt.Errorf("read verdict file: %w", err)
	}
	var v verdict
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("parse verdict file: %w", err)
	}
	return &v, nil
}

// evaluateDecision resolves a decision to one of its declared verdicts. For an
// `llm` decision the verdict comes from the file the reviewer agent wrote and is
// validated against the closed verdict set — the engine reads a verdict, it never
// judges. `exec` decisions are out of Phase 2a scope.
func (e *Engine) evaluateDecision(task *store.Task, name string) (string, error) {
	d, ok := e.wf.Decisions[name]
	if !ok {
		return "", fmt.Errorf("decision %q not declared", name)
	}
	switch d.Impl.Type {
	case "llm":
		v, err := readVerdict(e.taskDir, task.ID)
		if err != nil {
			return "", fmt.Errorf("decision %q: %w", name, err)
		}
		if !contains(d.Verdicts, v.Verdict) {
			return "", fmt.Errorf("decision %q: verdict %q is not one of the declared verdicts %v", name, v.Verdict, d.Verdicts)
		}
		e.log.Info("decision", "task", task.ID, "decision", name, "verdict", v.Verdict)
		return v.Verdict, nil
	default:
		return "", fmt.Errorf("decision %q: impl type %q not supported (Phase 2a implements llm)", name, d.Impl.Type)
	}
}

// decisionForState returns the decision a state's agent.done transition evaluates
// (empty if it gate-evaluates or has no such transition). This links the state's
// spawned agent (the reviewer) to the verdict it must produce.
func decisionForState(st config.State) string {
	if t := findEventTransition(st, "agent.done"); t != nil {
		return t.DecisionRef()
	}
	return ""
}

// reviewerTask builds the reviewer's context file (the decision rubric + a PR
// pointer) and a single-line kickoff instructing it to write the verdict file.
func (e *Engine) reviewerTask(task *store.Task, decisionName string) (taskFile, kickoff string, err error) {
	d := e.wf.Decisions[decisionName]
	rubric, err := e.readRubric(d.Impl.Rubric)
	if err != nil {
		return "", "", fmt.Errorf("decision %q: %w", decisionName, err)
	}
	path := filepath.Join(e.taskDir, "review-task-"+task.ID+".md")
	body := fmt.Sprintf("%s\n\n## PR under review\n\nPR #%d on branch %s. Read the changes with `gh pr diff %d`.\n",
		rubric, prNum(task), task.Branch, prNum(task))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", "", fmt.Errorf("write review task file: %w", err)
	}
	vp := verdictPath(e.taskDir, task.ID)
	kickoff = fmt.Sprintf(
		"Review PR #%d (branch %s) following the rubric in %s. When done, write your verdict as JSON {\"verdict\": one of %v, \"feedback\": \"...\"} to %s. Stop when the verdict file is written.",
		prNum(task), task.Branch, path, d.Verdicts, vp)
	return path, kickoff, nil
}

// readRubric reads a decision rubric, resolving a relative path against the
// config dir (so a workflow ships its prompts/ alongside its yaml).
func (e *Engine) readRubric(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("decision has no rubric")
	}
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.configDir, rel)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read rubric %s: %w", p, err)
	}
	return string(b), nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
