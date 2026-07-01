package config

// GraphAnalysis is a human-auditable view of a validated workflow's structure:
// the transition graph, its strongly-connected components (cycles), and the
// terminal and side-effecting states. It surfaces the mechanism/policy split the
// engine already computes so the `plan` subcommand can render it.
type GraphAnalysis struct {
	Edges         map[string][]string // state -> targets (sorted)
	SCCs          [][]string          // strongly-connected components
	Terminal      []string            // states with a terminal verdict (sorted)
	SideEffecting []string            // states whose entry.action is side-effecting, e.g. merge_pr (sorted)
}

// Analyze builds the human-auditable graph view of a validated workflow. It
// reuses buildGraph/tarjanSCC and the validator's sideEffectingActions set — no
// classification logic is duplicated.
func Analyze(wf *Workflow) GraphAnalysis {
	edges := buildGraph(wf)
	var terminal, side []string
	for _, name := range sortedKeys(wf.States) {
		s := wf.States[name]
		if s.Terminal != "" {
			terminal = append(terminal, name)
		}
		if s.Entry != nil && sideEffectingActions[s.Entry.Action] {
			side = append(side, name)
		}
	}
	return GraphAnalysis{
		Edges:         edges,
		SCCs:          tarjanSCC(edges),
		Terminal:      terminal,
		SideEffecting: side,
	}
}

// buildGraph builds the directed transition graph: state -> destination states.
// Action-only transitions contribute no edge (mirrors validate_workflow.py).
func buildGraph(wf *Workflow) map[string][]string {
	g := make(map[string][]string, len(wf.States))
	for name := range wf.States {
		g[name] = nil
	}
	for _, name := range sortedKeys(wf.States) {
		for _, t := range wf.States[name].Transitions {
			if t.To != "" {
				g[name] = append(g[name], t.To)
			} else if len(t.Branch) > 0 {
				for _, v := range sortedCopy(branchValues(t.Branch)) {
					g[name] = append(g[name], v)
				}
			}
		}
	}
	return g
}

func branchValues(b map[string]string) []string {
	out := make([]string, 0, len(b))
	for _, v := range b {
		out = append(out, v)
	}
	return out
}

// tarjanSCC returns the strongly connected components of graph. Nodes are
// visited in sorted order for deterministic output.
func tarjanSCC(graph map[string][]string) [][]string {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string
	counter := 0

	var strong func(v string)
	strong = func(v string) {
		index[v] = counter
		low[v] = counter
		counter++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range graph[v] {
			if _, seen := index[w]; !seen {
				strong(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] {
				if index[w] < low[v] {
					low[v] = index[w]
				}
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			out = append(out, comp)
		}
	}

	for _, v := range sortedKeys(graph) {
		if _, seen := index[v]; !seen {
			strong(v)
		}
	}
	return out
}
