package stats

import (
	"math"
	"sort"
)

// OutlierResult contains the result of outlier detection for a single data point.
type OutlierResult struct {
	Index      int
	Value      float64
	Percentile float64 // what percentile this value falls at (0-100)
	IsOutlier  bool
}

// LogIQROutliers detects abnormally slow values using the IQR method on log-transformed data.
// Only flags the upper tail — unusually fast runs are not interesting for CI analysis.
// The multiplier controls sensitivity (standard: 1.5, conservative: 3.0).
func LogIQROutliers(data []float64, multiplier float64) (outliers []OutlierResult, upperFence float64) {
	if len(data) < 5 {
		return nil, 0
	}

	// Log-transform (skip zero/negative values)
	logData := make([]float64, 0, len(data))
	validIdx := make([]int, 0, len(data))
	for i, v := range data {
		if v > 0 {
			logData = append(logData, math.Log(v))
			validIdx = append(validIdx, i)
		}
	}

	if len(logData) < 5 {
		return nil, 0
	}

	_, q3, iqr := IQR(logData)
	logUpper := q3 + multiplier*iqr

	// Back-transform fence to original scale
	upperFence = math.Exp(logUpper)

	// Compute percentile ranks for reporting
	sorted := make([]float64, len(data))
	copy(sorted, data)
	sort.Float64s(sorted)

	for i, idx := range validIdx {
		v := data[idx]
		logV := logData[i]
		if logV > logUpper {
			outliers = append(outliers, OutlierResult{
				Index:      idx,
				Value:      v,
				Percentile: percentileRank(sorted, v),
				IsOutlier:  true,
			})
		}
	}

	return outliers, upperFence
}

// MADOutliers detects abnormally slow values using Median Absolute Deviation.
// Only flags the upper tail. The threshold is applied to the modified z-score (commonly 3.5).
func MADOutliers(data []float64, threshold float64) (outliers []OutlierResult) {
	if len(data) < 5 {
		return nil
	}

	med := Median(data)

	// Compute absolute deviations
	absDevs := make([]float64, len(data))
	for i, v := range data {
		absDevs[i] = math.Abs(v - med)
	}
	mad := Median(absDevs)

	if mad == 0 {
		// All values are identical (or nearly so)
		return nil
	}

	// 1.4826 makes MAD consistent with stddev for normal distributions
	const consistency = 1.4826

	sorted := make([]float64, len(data))
	copy(sorted, data)
	sort.Float64s(sorted)

	for i, v := range data {
		modifiedZ := (v - med) / (consistency * mad)
		if modifiedZ > threshold {
			outliers = append(outliers, OutlierResult{
				Index:      i,
				Value:      v,
				Percentile: percentileRank(sorted, v),
				IsOutlier:  true,
			})
		}
	}

	return outliers
}

// ClampOutliers replaces isolated extreme values with the nearest fence value.
// Uses the IQR method: values beyond Q3 + multiplier×IQR (or below Q1 - multiplier×IQR)
// are clamped to the fence. This prevents single extreme outliers from poisoning
// downstream analysis while preserving genuine level shifts (which move the IQR).
func ClampOutliers(data []float64, multiplier float64) []float64 {
	if len(data) < 5 {
		return data
	}

	q1, q3, iqr := IQR(data)
	if iqr == 0 {
		return data
	}

	lower := q1 - multiplier*iqr
	upper := q3 + multiplier*iqr

	result := make([]float64, len(data))
	for i, v := range data {
		switch {
		case v > upper:
			result[i] = upper
		case v < lower:
			result[i] = lower
		default:
			result[i] = v
		}
	}
	return result
}

// percentileRank returns what percentile a value falls at in a sorted slice (0-100).
func percentileRank(sorted []float64, value float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	count := 0
	for _, v := range sorted {
		if v < value {
			count++
		}
	}
	return float64(count) / float64(n) * 100
}
