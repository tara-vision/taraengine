package llamaserver

import (
	"slices"
	"strings"
)

// maxCompared bounds the part of an unknown name that is compared with the catalog: a model name has at most 64
// characters, and the cost of a comparison grows with the length of both names.
const maxCompared = 128

// nearestNames are up to n of the names closest to name by edit distance, nearest first and in name order
// between equals. Case is ignored, so a name typed in capitals finds its model.
func nearestNames(name string, names []string, n int) []string {
	target := strings.ToLower(name[:min(len(name), maxCompared)])
	type scored struct {
		name     string
		distance int
	}
	all := make([]scored, 0, len(names))
	for _, candidate := range names {
		all = append(all, scored{candidate, editDistance(target, strings.ToLower(candidate))})
	}
	slices.SortFunc(all, func(a, b scored) int {
		if a.distance != b.distance {
			return a.distance - b.distance
		}
		return strings.Compare(a.name, b.name)
	})
	out := make([]string, 0, n)
	for _, s := range all[:min(n, len(all))] {
		out = append(out, s.name)
	}
	return out
}

// editDistance is the Levenshtein distance between a and b: the fewest inserted, deleted or replaced bytes
// that turn one into the other.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	current := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
