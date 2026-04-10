// Package stats provides statistical functions for CI performance analysis.
package stats

import (
	"math"
	"sort"
)

// Median returns the median of a float64 slice. The input is not modified.
func Median(data []float64) float64 {
	return Percentile(data, 50)
}

// Percentile returns the p-th percentile (0-100) using linear interpolation.
// The input is not modified.
func Percentile(data []float64, p float64) float64 {
	if len(data) == 0 {
		return 0
	}
	sorted := make([]float64, len(data))
	copy(sorted, data)
	sort.Float64s(sorted)

	idx := p / 100 * float64(len(sorted)-1)
	lower := int(idx)
	if lower >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lower)
	return sorted[lower] + frac*(sorted[lower+1]-sorted[lower])
}

// IQR returns Q1, Q3, and the interquartile range.
func IQR(data []float64) (q1, q3, iqr float64) {
	q1 = Percentile(data, 25)
	q3 = Percentile(data, 75)
	iqr = q3 - q1
	return q1, q3, iqr
}

// Mean returns the arithmetic mean.
func Mean(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	var sum float64
	for _, v := range data {
		sum += v
	}
	return sum / float64(len(data))
}

// Stddev returns the sample standard deviation.
func Stddev(data []float64) float64 {
	if len(data) < 2 {
		return 0
	}
	m := Mean(data)
	var ss float64
	for _, v := range data {
		d := v - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(data)-1))
}
