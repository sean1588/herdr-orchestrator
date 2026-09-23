// Package classify answers one question about an agent pane whose bytes have
// stopped moving: what does the stillness mean?
//
// The engine's no-progress bound can only tell that a pane is static, never
// why. A pane parked on a permission prompt and a pane inside a long test run
// look identical to a digest; they need opposite responses. A PaneClassifier
// labels the tail the engine already read, and the engine decides what to do.
//
// The engine depends only on the PaneClassifier interface (a small seam at the
// boundary, like notify/exec/github). Jev asks a model and answers every label;
// Heuristic needs no key and recognises only Claude Code's permission prompts. A
// classifier failure must never change what the engine would have done without
// one, so implementations return errors rather than guesses.
package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Activity is what the agent in a static pane is doing.
type Activity string

const (
	Working            Activity = "working"
	AwaitingPermission Activity = "awaiting_permission"
	AwaitingAnswer     Activity = "awaiting_answer"
	Finished           Activity = "finished"
	Crashed            Activity = "crashed"
)

// criteria is the closed label set sent to the model, each with the wording it
// judges against. The model cannot answer outside it.
var criteria = map[Activity]string{
	Working:            "Actively executing commands, editing files, running tests, or producing output",
	AwaitingPermission: "Stopped on a tool-permission or trust prompt that requires a keypress to continue",
	AwaitingAnswer:     "Stopped on a question addressed to the human user, not a permission prompt",
	Finished:           "Reported that the task is complete and is sitting at an idle prompt",
	Crashed:            "An error, stack trace, or bare shell prompt with no agent running",
}

// Threshold is the probability an answer must reach before the engine acts on
// it. Live probes returned 0.97-0.99 on unambiguous tails; 0.9 leaves headroom
// while excluding genuinely ambiguous ones. Deliberately not configurable: a
// knob to express it is a knob to loosen it.
const Threshold = 0.9

// Result is a classification: the chosen label and the probability the model
// assigned to it.
type Result struct {
	Activity   Activity
	Confidence float64
}

// Confident reports whether r is sure enough to act on.
func (r Result) Confident() bool { return r.Confidence >= Threshold }

// PaneClassifier labels a static pane tail. Implementations must honor ctx and
// bound their own latency: the engine calls this inside its drive loop.
type PaneClassifier interface {
	Classify(ctx context.Context, paneTail string) (Result, error)
}

// Model is the pinned Jev version. The ~typesafe/jev-latest alias resolves to
// zero endpoints on OpenRouter, so an unpinned id does not work.
const Model = "typesafe/jev-1.13"

// RequestTimeout bounds one classification. Jev answers in 70-500ms; the call
// runs inside the drive loop, so a hung endpoint must cost seconds, not the
// rest of the drive.
const RequestTimeout = 5 * time.Second

// KeyEnv is the environment variable holding the OpenRouter API key.
const KeyEnv = "OPENROUTER_API_KEY"

// client is shared so calls reuse one connection pool. It always carries
// RequestTimeout: the drive loop's context has no deadline of its own.
var client = &http.Client{Timeout: RequestTimeout}

// Jev classifies through TypeSafe's Jev, a non-generative choice model, via
// OpenRouter's native /systemone endpoint.
type Jev struct {
	URL    string              // e.g. https://openrouter.ai/api/v1/systemone
	Getenv func(string) string // reads KeyEnv; nil => os.Getenv
}

type jevQuestion struct {
	Type         string              `json:"type"`
	Instructions string              `json:"instructions"`
	Criteria     map[Activity]string `json:"criteria"`
}

type jevRequest struct {
	Model     string                 `json:"model"`
	State     map[string]string      `json:"state"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevResponse struct {
	Answers map[string]struct {
		Choice        Activity             `json:"choice"`
		Probabilities map[Activity]float64 `json:"probabilities"`
	} `json:"answers"`
	Usage struct {
		Cost float64 `json:"cost"`
	} `json:"usage"`
}

// Classify sends paneTail to Jev and returns its answer. Any transport, status,
// or shape problem is an error — including a label outside the declared set,
// which the engine must never be asked to interpret.
func (j Jev) Classify(ctx context.Context, paneTail string) (Result, error) {
	r, _, err := j.classify(ctx, paneTail)
	return r, err
}

// classify is Classify plus the call's reported cost, which the live fixture
// test logs so the per-call price stays visible.
func (j Jev) classify(ctx context.Context, paneTail string) (Result, float64, error) {
	getenv := j.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	key := getenv(KeyEnv)
	if key == "" {
		return Result{}, 0, fmt.Errorf("classify: %s is not set", KeyEnv)
	}

	body, err := json.Marshal(jevRequest{
		Model: Model,
		State: map[string]string{"pane_tail": paneTail},
		Questions: map[string]jevQuestion{"activity": {
			Type:         "choice",
			Instructions: "What is the coding agent in this terminal pane doing right now",
			Criteria:     criteria,
		}},
	})
	if err != nil {
		return Result{}, 0, fmt.Errorf("classify: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.URL, bytes.NewReader(body))
	if err != nil {
		return Result{}, 0, fmt.Errorf("classify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, 0, fmt.Errorf("classify: POST %s: %w", j.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Result{}, 0, fmt.Errorf("classify: POST %s: status %d: %s", j.URL, resp.StatusCode, snippet)
	}

	var out jevResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, 0, fmt.Errorf("classify: decode response: %w", err)
	}
	ans, ok := out.Answers["activity"]
	if !ok {
		return Result{}, 0, fmt.Errorf("classify: response has no activity answer")
	}
	if _, known := criteria[ans.Choice]; !known {
		return Result{}, 0, fmt.Errorf("classify: unknown activity %q", ans.Choice)
	}
	return Result{Activity: ans.Choice, Confidence: ans.Probabilities[ans.Choice]}, out.Usage.Cost, nil
}
