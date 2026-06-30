package config

import "sort"

// sortedKeys returns the keys of m in ascending order, for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedCopy returns a sorted copy of s without mutating the input.
func sortedCopy(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// contains reports whether s includes x.
func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

// sameSet reports whether a and b contain the same set of values.
func sameSet(a, b []string) bool {
	sa := make(map[string]struct{}, len(a))
	sb := make(map[string]struct{}, len(b))
	for _, x := range a {
		sa[x] = struct{}{}
	}
	for _, x := range b {
		sb[x] = struct{}{}
	}
	if len(sa) != len(sb) {
		return false
	}
	for x := range sa {
		if _, ok := sb[x]; !ok {
			return false
		}
	}
	return true
}
