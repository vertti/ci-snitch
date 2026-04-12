package stats

import (
	"math"
	"math/rand/v2"
	"sort"
)

const (
	// maxExactN is the maximum combined sample size for exact enumeration.
	maxExactN = 20
	// permutationReps is the number of random permutations for the Monte Carlo test.
	permutationReps = 10000
)

// MannWhitneyU performs a two-sided Mann-Whitney U test comparing two samples.
// Uses three strategies depending on sample size:
//   - Exact enumeration when n1+n2 <= 20 (feasible combinatorics)
//   - Monte Carlo permutation test when min(n1,n2) <= 20 (small sample, exact infeasible)
//   - Normal approximation when both n1,n2 > 20
//
// A small p-value (< 0.05) indicates the two samples likely come from different distributions.
func MannWhitneyU(sample1, sample2 []float64) (u float64, pValue float64) {
	n1 := len(sample1)
	n2 := len(sample2)
	if n1 == 0 || n2 == 0 {
		return 0, 1
	}

	u = computeU(sample1, sample2)

	n := n1 + n2
	switch {
	case n <= maxExactN:
		pValue = exactPValue(n1, n2, u)
	case min(n1, n2) <= 20:
		pValue = permutationPValue(sample1, sample2, u)
	default:
		pValue = normalApproxPValue(n1, n2, u)
	}

	return u, pValue
}

func computeU(sample1, sample2 []float64) float64 {
	n1 := len(sample1)

	type ranked struct {
		value float64
		group int // 0 = sample1, 1 = sample2
		rank  float64
	}

	combined := make([]ranked, 0, n1+len(sample2))
	for _, v := range sample1 {
		combined = append(combined, ranked{value: v, group: 0})
	}
	for _, v := range sample2 {
		combined = append(combined, ranked{value: v, group: 1})
	}

	sort.Slice(combined, func(i, j int) bool {
		return combined[i].value < combined[j].value
	})

	// Assign ranks with tie handling (average rank for ties)
	i := 0
	for i < len(combined) {
		j := i + 1
		for j < len(combined) && combined[j].value == combined[i].value {
			j++
		}
		avgRank := float64(i+j+1) / 2.0 // average of 1-based ranks i+1..j
		for k := i; k < j; k++ {
			combined[k].rank = avgRank
		}
		i = j
	}

	// Sum ranks for sample 1
	r1 := 0.0
	for _, r := range combined {
		if r.group == 0 {
			r1 += r.rank
		}
	}

	fn1 := float64(n1)
	fn2 := float64(len(sample2))

	u1 := r1 - fn1*(fn1+1)/2
	u2 := fn1*fn2 - u1
	return math.Min(u1, u2)
}

// exactPValue computes the exact two-sided p-value by enumerating all
// possible U statistics from permutations of group assignments.
// Uses the combinatorial counting approach: iterate over all ways to choose
// n1 items from n1+n2 positions and compute the proportion with U <= observed.
func exactPValue(n1, n2 int, observedU float64) float64 {
	n := n1 + n2
	total := binomial(n, n1)
	if total == 0 {
		return 1
	}

	// Count permutations where U <= observedU.
	// We enumerate all C(n, n1) ways to assign ranks to group 1.
	count := 0.0
	enumerateCombs(n, n1, func(ranks []int) {
		// Compute rank sum for this assignment
		r1 := 0.0
		for _, r := range ranks {
			r1 += float64(r + 1) // convert 0-based index to 1-based rank
		}
		fn1 := float64(n1)
		fn2 := float64(n2)
		u1 := r1 - fn1*(fn1+1)/2
		u2 := fn1*fn2 - u1
		u := math.Min(u1, u2)
		if u <= observedU {
			count++
		}
	})

	return count / total
}

// enumerateCombs calls fn for each combination of k indices from [0, n).
func enumerateCombs(n, k int, fn func([]int)) {
	indices := make([]int, k)
	for i := range k {
		indices[i] = i
	}

	for {
		fn(indices)

		// Find rightmost index that can be incremented
		i := k - 1
		for i >= 0 && indices[i] == n-k+i {
			i--
		}
		if i < 0 {
			break
		}
		indices[i]++
		for j := i + 1; j < k; j++ {
			indices[j] = indices[j-1] + 1
		}
	}
}

// binomial returns C(n, k) as a float64.
func binomial(n, k int) float64 {
	if k > n || k < 0 {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	result := 1.0
	for i := range k {
		result *= float64(n-i) / float64(i+1)
	}
	return result
}

// permutationPValue estimates the two-sided p-value by randomly shuffling
// group assignments and computing the proportion of permutations where
// U <= observed U.
func permutationPValue(sample1, sample2 []float64, observedU float64) float64 {
	n1 := len(sample1)
	combined := make([]float64, 0, n1+len(sample2))
	combined = append(combined, sample1...)
	combined = append(combined, sample2...)

	count := 0
	for range permutationReps {
		rand.Shuffle(len(combined), func(i, j int) {
			combined[i], combined[j] = combined[j], combined[i]
		})
		u := computeU(combined[:n1], combined[n1:])
		if u <= observedU {
			count++
		}
	}

	return float64(count) / float64(permutationReps)
}

func normalApproxPValue(n1, n2 int, u float64) float64 {
	fn1 := float64(n1)
	fn2 := float64(n2)

	mu := fn1 * fn2 / 2
	sigma := math.Sqrt(fn1 * fn2 * (fn1 + fn2 + 1) / 12)
	if sigma == 0 {
		return 1
	}

	z := math.Abs((u - mu) / sigma)
	return 2 * (1 - normalCDF(z))
}

// normalCDF approximates the standard normal cumulative distribution function.
func normalCDF(z float64) float64 {
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}
