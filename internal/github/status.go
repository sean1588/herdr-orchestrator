package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// prStatusFields is the `gh pr view --json` field set backing PRStatus.
const prStatusFields = "state,statusCheckRollup,reviewDecision,reviews,mergeable,mergeStateStatus"

// prViewStatus mirrors the gh pr view JSON we read for merge gating.
type prViewStatus struct {
	State             string `json:"state"`
	ReviewDecision    string `json:"reviewDecision"`
	Mergeable         string `json:"mergeable"`
	MergeStateStatus  string `json:"mergeStateStatus"`
	StatusCheckRollup []struct {
		TypeName   string `json:"__typename"`
		Status     string `json:"status"`     // CheckRun: QUEUED | IN_PROGRESS | COMPLETED
		Conclusion string `json:"conclusion"` // CheckRun: SUCCESS | FAILURE | NEUTRAL | ...
		State      string `json:"state"`      // StatusContext: SUCCESS | PENDING | FAILURE | ERROR
	} `json:"statusCheckRollup"`
	Reviews []struct {
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		State       string `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | ...
		SubmittedAt string `json:"submittedAt"`
	} `json:"reviews"`
}

// PRStatus runs `gh pr view <pr> --json <merge-gate fields>` in repoDir and folds
// the result into a PRStatus. Using `gh pr view` (not `gh pr checks`) keeps the
// exit code 0 even when checks are pending or failing, so failures surface as
// data rather than a runner error.
func (g *GH) PRStatus(ctx context.Context, repoDir string, pr int) (*PRStatus, error) {
	out, err := g.run.Run(ctx, repoDir, "gh", "pr", "view", strconv.Itoa(pr), "--json", prStatusFields)
	if err != nil {
		return nil, fmt.Errorf("gh pr view %d (status): %w", pr, err)
	}
	var v prViewStatus
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("parse gh pr view status for %d: %w", pr, err)
	}

	s := &PRStatus{
		State:            v.State,
		ReviewDecision:   v.ReviewDecision,
		Mergeable:        v.Mergeable,
		MergeStateStatus: v.MergeStateStatus,
	}
	for _, c := range v.StatusCheckRollup {
		s.ChecksTotal++
		switch classifyCheck(c.Status, c.Conclusion, c.State) {
		case checkFail:
			s.ChecksFailed++
		case checkPending:
			s.ChecksPending++
		}
	}

	// Count distinct authors whose latest *decisive* review is an approval. Only
	// decisive reviews set a reviewer's standing; a later COMMENTED (or PENDING)
	// review is advisory and does not revoke a prior approval — mirroring how
	// GitHub derives reviewDecision.
	latest := map[string]struct{ state, at string }{}
	for _, r := range v.Reviews {
		login := r.Author.Login
		if login == "" || !decisiveReview(r.State) {
			continue
		}
		if cur, ok := latest[login]; !ok || r.SubmittedAt >= cur.at {
			latest[login] = struct{ state, at string }{r.State, r.SubmittedAt}
		}
	}
	for _, l := range latest {
		if l.state == "APPROVED" {
			s.ApprovedReviews++
		}
	}
	return s, nil
}

// decisiveReview reports whether a review state changes the reviewer's standing.
// APPROVED, CHANGES_REQUESTED, and DISMISSED are decisive; COMMENTED and PENDING
// are advisory and never supersede a prior decisive review.
func decisiveReview(state string) bool {
	switch state {
	case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
		return true
	default:
		return false
	}
}

type checkClass int

const (
	checkPass checkClass = iota
	checkFail
	checkPending
)

// classifyCheck folds a statusCheckRollup entry into pass/fail/pending. A rollup
// holds two shapes: StatusContext (state set) and CheckRun (status+conclusion).
func classifyCheck(status, conclusion, state string) checkClass {
	if state != "" { // StatusContext
		switch state {
		case "SUCCESS":
			return checkPass
		case "FAILURE", "ERROR":
			return checkFail
		default: // PENDING, EXPECTED
			return checkPending
		}
	}
	// CheckRun
	if status != "COMPLETED" { // QUEUED, IN_PROGRESS, WAITING, PENDING, REQUESTED
		return checkPending
	}
	switch conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return checkPass
	default: // FAILURE, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STARTUP_FAILURE, STALE
		return checkFail
	}
}
