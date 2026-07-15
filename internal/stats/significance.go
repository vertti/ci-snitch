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
// Uses a fresh random source for the permutation path, so results are
// non-deterministic. For deterministic tests use MannWhitneyURand.
func MannWhitneyU(sample1, sample2 []float64) (u, pValue float64) {
	return MannWhitneyURand(sample1, sample2, nil)
}

// MannWhitneyURand performs a two-sided Mann-Whitney U test comparing two samples.
// Uses three strategies depending on sample size:
//   - Exact enumeration when n1+n2 <= 20 (feasible combinatorics)
//   - Monte Carlo permutation test when min(n1,n2) <= 20 (small sample, exact infeasible)
//   - Normal approximation when both n1,n2 > 20
//
// If rng is nil, a fresh random source is used.
// A small p-value (< 0.05) indicates the two samples likely come from different distributions.
func MannWhitneyURand(sample1, sample2 []float64, rng *rand.Rand) (u, pValue float64) {
	n1 := len(sample1)
	n2 := len(sample2)
	if n1 == 0 || n2 == 0 {
		return 0, 1
	}

	u = computeU(sample1, sample2)

	n := n1 + n2
	switch {
	case n <= maxExactN:
		pValue = exactPValue(sample1, sample2, u)
	case min(n1, n2) <= 20:
		pValue = permutationPValue(sample1, sample2, u, rng)
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
// C(n, n1) assignments of the observed VALUES to group 1 and recomputing U
// (with average-rank tie handling) for each. Enumerating rank positions
// 1..n instead — as if the data were untied — is anti-conservative with
// tied data: second-resolution CI durations tie constantly, and the
// rank-only enumeration reported p=0.31 where the tie-aware truth is 0.52.
func exactPValue(sample1, sample2 []float64, observedU float64) float64 {
	n1, n2 := len(sample1), len(sample2)
	n := n1 + n2
	combined := make([]float64, 0, n)
	combined = append(combined, sample1...)
	combined = append(combined, sample2...)

	s1 := make([]float64, n1)
	s2 := make([]float64, n2)
	inS1 := make([]bool, n)

	total := 0.0
	count := 0.0
	enumerateCombs(n, n1, func(idx []int) {
		total++
		for i := range inS1 {
			inS1[i] = false
		}
		for j, i := range idx {
			s1[j] = combined[i]
			inS1[i] = true
		}
		k := 0
		for i := range combined {
			if !inS1[i] {
				s2[k] = combined[i]
				k++
			}
		}
		if computeU(s1, s2) <= observedU+1e-9 {
			count++
		}
	})

	if total == 0 {
		return 1
	}
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

// permutationPValue estimates the two-sided p-value by randomly shuffling
// group assignments and computing the proportion of permutations where
// U <= observed U.
func permutationPValue(sample1, sample2 []float64, observedU float64, rng *rand.Rand) float64 {
	if rng == nil {
		rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) //nolint:gosec // statistical shuffling, not crypto
	}

	n1 := len(sample1)
	combined := make([]float64, 0, n1+len(sample2))
	combined = append(combined, sample1...)
	combined = append(combined, sample2...)

	count := 0
	for range permutationReps {
		rng.Shuffle(len(combined), func(i, j int) {
			combined[i], combined[j] = combined[j], combined[i]
		})
		u := computeU(combined[:n1], combined[n1:])
		if u <= observedU {
			count++
		}
	}

	// +1 smoothing: a Monte-Carlo p-value of exactly 0 overstates the
	// evidence — the observed arrangement itself is always one permutation.
	return float64(count+1) / float64(permutationReps+1)
}

func normalApproxPValue(n1, n2 int, u float64) float64 {
	fn1 := float64(n1)
	fn2 := float64(n2)

	mu := fn1 * fn2 / 2
	sigma := math.Sqrt(fn1 * fn2 * (fn1 + fn2 + 1) / 12)
	if sigma == 0 {
		return 1
	}

	// Continuity correction: U is discrete, the normal is continuous.
	z := (math.Abs(u-mu) - 0.5) / sigma
	if z < 0 {
		z = 0
	}
	return 2 * (1 - normalCDF(z))
}

// normalCDF approximates the standard normal cumulative distribution function.
func normalCDF(z float64) float64 {
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}
