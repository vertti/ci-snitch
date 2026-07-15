package stats

import "sort"

// BenjaminiHochberg converts p-values into q-values (false-discovery-rate
// adjusted p-values). Testing many hypotheses at a fixed alpha guarantees
// false positives — with 20 stable jobs at alpha=0.05, one "significant"
// regression is expected by chance; comparing q-values against alpha
// controls the expected fraction of false discoveries instead.
// The returned slice matches the input order; q >= p always holds.
func BenjaminiHochberg(ps []float64) []float64 {
	n := len(ps)
	if n == 0 {
		return nil
	}

	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return ps[idx[a]] < ps[idx[b]] })

	qs := make([]float64, n)
	minSoFar := 1.0
	for rank := n; rank >= 1; rank-- {
		i := idx[rank-1]
		q := ps[i] * float64(n) / float64(rank)
		if q < minSoFar {
			minSoFar = q
		}
		qs[i] = minSoFar
	}
	return qs
}
