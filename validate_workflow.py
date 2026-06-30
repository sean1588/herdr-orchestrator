#!/usr/bin/env python3
"""Validate a Herdr Orchestrator workflow config.

Usage: python3 validate_workflow.py <config.yaml> [--schema workflow.schema.json]

  1. JSON Schema (shape)           -> workflow.schema.json
  2. Semantic invariants (safety)  -> enforced below

Exit 0 if no errors (warnings allowed), 1 otherwise.

NOTE: This is the REFERENCE implementation of the semantic invariants. The
authoritative validator used by the daemon is the Go port in
internal/config/validate.go, which is kept behaviorally equivalent to this file.
The embedded schema lives at internal/config/workflow.schema.json.
"""
import argparse
import json
import os
import sys

import yaml
from jsonschema import Draft202012Validator

AUTHORITATIVE_GATE_TYPES = {"github_pr", "github_checks", "github_reviews", "github_mergeable"}
SIDE_EFFECTING_ACTIONS = {"merge_pr"}


def tarjan_sccs(graph):
    index, low, onstack, stack, out, counter = {}, {}, {}, [], [], [0]
    sys.setrecursionlimit(10000)

    def strong(v):
        index[v] = low[v] = counter[0]
        counter[0] += 1
        stack.append(v)
        onstack[v] = True
        for w in graph.get(v, []):
            if w not in index:
                strong(w)
                low[v] = min(low[v], low[w])
            elif onstack.get(w):
                low[v] = min(low[v], index[w])
        if low[v] == index[v]:
            comp = []
            while True:
                w = stack.pop()
                onstack[w] = False
                comp.append(w)
                if w == v:
                    break
            out.append(comp)

    for v in list(graph):
        if v not in index:
            strong(v)
    return out


def trigger_ref(t, kind):
    for src in (t.get("when", {}), t.get("evaluate", {})):
        if kind in src:
            return src[kind]
    return None


def gate_names(ref):
    if ref is None:
        return []
    return list(ref) if isinstance(ref, list) else [ref]


def semantic_checks(cfg, errors, warnings):
    states = cfg.get("states", {})
    roles = cfg.get("roles", {})
    gates = cfg.get("gates", {})
    decisions = cfg.get("decisions", {})
    retry_caps = cfg.get("policies", {}).get("retry_caps", {})

    for sname, s in states.items():
        entry = s.get("entry", {})
        for k in ("spawn", "resume"):
            if k in entry and entry[k] not in roles:
                errors.append(f"state '{sname}': entry.{k} references unknown role '{entry[k]}'")

        for i, t in enumerate(s.get("transitions", [])):
            where = f"state '{sname}' transition[{i}]"
            dec = trigger_ref(t, "decision")
            gts = gate_names(trigger_ref(t, "gate"))
            if dec is not None and dec not in decisions:
                errors.append(f"{where}: references unknown decision '{dec}'")
            for g in gts:
                if g not in gates:
                    errors.append(f"{where}: references unknown gate '{g}'")

            targets = [t["to"]] if "to" in t else list(t.get("branch", {}).values())
            for tgt in targets:
                if tgt not in states:
                    errors.append(f"{where}: targets unknown state '{tgt}'")

            if "branch" in t:
                keys = set(t["branch"].keys())
                if dec is not None:
                    verdicts = set(decisions.get(dec, {}).get("verdicts", []))
                    if keys != verdicts:
                        errors.append(
                            f"{where}: decision '{dec}' branch keys {sorted(keys)} "
                            f"must exactly cover verdicts {sorted(verdicts)}")
                elif gts:
                    if keys != {"pass", "fail"}:
                        errors.append(f"{where}: gate branch keys {sorted(keys)} must be ['fail', 'pass']")
                else:
                    errors.append(f"{where}: 'branch' requires a decision or gate trigger/evaluate")
            elif (dec is not None or gts) and "action" not in t:
                errors.append(f"{where}: decision/gate transition must have a 'branch'")

    for gname, g in gates.items():
        if g.get("type") not in AUTHORITATIVE_GATE_TYPES:
            errors.append(f"gate '{gname}': type '{g.get('type')}' is not an authoritative source "
                          f"(allowed: {sorted(AUTHORITATIVE_GATE_TYPES)})")

    side = {n for n, s in states.items()
            if s.get("entry", {}).get("action") in SIDE_EFFECTING_ACTIONS}
    for sname, s in states.items():
        for i, t in enumerate(s.get("transitions", [])):
            tgts = [t["to"]] if "to" in t else list(t.get("branch", {}).values())
            for tgt in tgts:
                if tgt in side and not gate_names(trigger_ref(t, "gate")):
                    errors.append(
                        f"state '{sname}' transition[{i}]: enters side-effecting state '{tgt}' "
                        f"without a gate (merge must be gate-evaluated, never a decision/event)")

    graph = {n: [] for n in states}
    for sname, s in states.items():
        for t in s.get("transitions", []):
            if "to" in t:
                graph[sname].append(t["to"])
            elif "branch" in t:
                graph[sname].extend(t["branch"].values())
    for comp in tarjan_sccs(graph):
        cyclic = len(comp) > 1 or (len(comp) == 1 and comp[0] in graph.get(comp[0], []))
        if not cyclic:
            continue
        capped = any(
            n in retry_caps or any("timeout" in t.get("when", {}) for t in states[n].get("transitions", []))
            for n in comp)
        if not capped:
            errors.append(f"cycle {sorted(comp)} has no retry cap or timeout -> non-terminating loop")

    for sname, s in states.items():
        if "terminal" in s:
            if s.get("transitions"):
                warnings.append(f"terminal state '{sname}' also declares transitions (ignored)")
            continue
        if not s.get("transitions") and "wait_for" not in s:
            errors.append(f"non-terminal state '{sname}' has no exit (no transitions, no wait_for)")

    entry_state = cfg.get("entry_state")
    if entry_state is None:
        warnings.append("no entry_state declared; cannot check reachability")
    elif entry_state not in states:
        errors.append(f"entry_state '{entry_state}' is not a declared state")
    else:
        seen, stack = set(), [entry_state]
        while stack:
            n = stack.pop()
            if n in seen:
                continue
            seen.add(n)
            stack.extend(graph.get(n, []))
        for n in states:
            if n not in seen and "wait_for" not in states[n]:
                warnings.append(f"state '{n}' is unreachable from entry_state '{entry_state}'")

    for sname, s in states.items():
        e = s.get("entry", {})
        if ("spawn" in e or "resume" in e) and not any(
                "timeout" in t.get("when", {}) for t in s.get("transitions", [])):
            warnings.append(f"state '{sname}' spawns/resumes an agent but has no timeout transition")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("config")
    ap.add_argument("--schema", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), "workflow.schema.json"))
    args = ap.parse_args()

    with open(args.schema) as f:
        schema = json.load(f)
    with open(args.config) as f:
        cfg = yaml.safe_load(f)

    errors, warnings = [], []
    v = Draft202012Validator(schema)
    schema_errors = sorted(v.iter_errors(cfg), key=lambda e: list(e.path))
    for e in schema_errors:
        loc = "/".join(str(p) for p in e.path) or "(root)"
        errors.append(f"schema: {loc}: {e.message}")

    if not schema_errors:
        semantic_checks(cfg, errors, warnings)

    for w in warnings:
        print(f"  WARN  {w}")
    for er in errors:
        print(f"  ERROR {er}")

    if errors:
        print(f"\nFAIL: {len(errors)} error(s), {len(warnings)} warning(s)")
        sys.exit(1)
    print(f"\nOK: valid ({len(warnings)} warning(s))")
    sys.exit(0)


if __name__ == "__main__":
    main()
