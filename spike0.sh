#!/usr/bin/env bash
set -euo pipefail

# Spike 0 - prove the herdr <-> GitHub loop.
# Run inside a herdr pane (so herdr talks to the live session). gh must be authed.
#   usage:  REPO_DIR=/path/to/repo ./spike0.sh <issue-number>
# Claude Code refuses --dangerously-skip-permissions as root; default launches plain claude
# and you approve git/gh in the pane. For unattended runs: run as a NON-root user.
#
# NOTE: This is the REFERENCE script. The Go herdr backend (internal/exec/herdr.go)
# wraps these same commands behind the ExecutionBackend interface.

REPO_DIR=$(cd "${REPO_DIR:-.}" && pwd)
BASE="${BASE:-main}"
N="${1:?usage: spike0.sh <issue-number>}"
AGENT_CMD="${AGENT_CMD:-claude}"
BRANCH="agent/issue-$N"
WT="$REPO_DIR/../wt-issue-$N"
TASK_FILE="/tmp/spike-task-$N.md"

# 1. issue -> task FILE (never send multi-line text through the terminal)
(cd "$REPO_DIR" && gh issue view "$N" --json title,body \
  --jq '"# " + .title + "\n\n" + .body') > "$TASK_FILE"

# 2. fresh worktree + herdr workspace (auto-clean any prior attempt)
git -C "$REPO_DIR" worktree remove "$WT" --force 2>/dev/null || true
git -C "$REPO_DIR" branch -D "$BRANCH" 2>/dev/null || true
git -C "$REPO_DIR" worktree add -b "$BRANCH" "$WT" "$BASE"
WT_ABS=$(cd "$WT" && pwd)
WS=$(herdr workspace create --cwd "$WT_ABS" --label "issue-$N")
PANE=$(printf '%s' "$WS" | python3 -c 'import sys,json;print(json.load(sys.stdin)["result"]["root_pane"]["pane_id"])')
echo "pane=$PANE worktree=$WT_ABS"

# 3. launch agent, then send ONE single-line kickoff pointing at the task file
herdr pane run "$PANE" "$AGENT_CMD"
sleep 12
KICKOFF="Read the task in $TASK_FILE and implement it on this branch ($BRANCH). Then commit, run 'git push -u origin $BRANCH', and open a PR with 'gh pr create --fill --base $BASE'. Stop when the PR is open."
herdr pane run "$PANE" "$KICKOFF"

# 4. wait for the agent to settle
t_start=$(date +%s)
herdr wait agent-status "$PANE" --status done --timeout 1800000 \
  || echo "WARN: not 'done' (blocked/timeout) - inspect: herdr pane read $PANE --source recent --lines 60"
t_done=$(date +%s)

# 5. detect the PR (the authoritative signal)
PR=""
for i in $(seq 1 30); do
  PR=$(cd "$REPO_DIR" && gh pr list --head "$BRANCH" --json number --jq '.[0].number' 2>/dev/null || true)
  [ -n "$PR" ] && break
  sleep 10
done
t_pr=$(date +%s)

if [ -n "$PR" ]; then
  echo "PR #$PR opened. agent-done at +$((t_done-t_start))s, PR detected at +$((t_pr-t_start))s (gap $((t_pr-t_done))s)."
else
  echo "No PR detected within timeout. Inspect: herdr pane read $PANE --source recent --lines 60"
fi
