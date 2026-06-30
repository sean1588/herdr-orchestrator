package config

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
