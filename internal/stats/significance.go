package stats

import (
	"math"
	"sort"
)

// MannWhitneyU performs a two-sided Mann-Whitney U test comparing two samples.
// Returns the U statistic and an approximate p-value (normal approximation, valid for n > 20).
// A small p-value (< 0.05) indicates the two samples likely come from different distributions.
func MannWhitneyU(sample1, sample2 []float64) (u float64, pValue float64) {
	n1 := len(sample1)
	n2 := len(sample2)
	if n1 == 0 || n2 == 0 {
		return 0, 1
	}

	type ranked struct {
		value float64
		group int // 0 = sample1, 1 = sample2
		rank  float64
	}

	combined := make([]ranked, 0, n1+n2)
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
	fn2 := float64(n2)

	u1 := r1 - fn1*(fn1+1)/2
	u2 := fn1*fn2 - u1
	u = math.Min(u1, u2)

	// Normal approximation for p-value
	mu := fn1 * fn2 / 2
	sigma := math.Sqrt(fn1 * fn2 * (fn1 + fn2 + 1) / 12)
	if sigma == 0 {
		return u, 1
	}

	z := math.Abs((u - mu) / sigma)
	// Two-sided p-value from standard normal
	pValue = 2 * (1 - normalCDF(z))
	return u, pValue
}

// normalCDF approximates the standard normal cumulative distribution function.
func normalCDF(z float64) float64 {
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}
