package stats

import (
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogIQROutliers_DetectsHighOutlier(t *testing.T) {
	// Generate log-normal-ish data with a clear outlier
	data := []float64{100, 105, 98, 102, 101, 99, 103, 97, 104, 100,
		101, 98, 102, 100, 103, 99, 101, 100, 102, 98,
		500} // outlier

	outliers, upper := LogIQROutliers(data, 1.5)
	require.NotEmpty(t, outliers, "should detect the 500 outlier")

	found500 := false
	for _, o := range outliers {
		if o.Value == 500 {
			found500 = true
			assert.Greater(t, o.Percentile, 90.0, "500 should be high percentile")
		}
	}
	assert.True(t, found500, "should have found 500 as outlier")
	assert.Less(t, upper, 500.0, "upper fence should be below 500")
}

func TestLogIQROutliers_NoOutliers(t *testing.T) {
	// Uniform-ish data, no outliers
	data := []float64{100, 102, 101, 103, 99, 100, 101, 102, 100, 101}

	outliers, _ := LogIQROutliers(data, 1.5)
	assert.Empty(t, outliers)
}

func TestLogIQROutliers_TooFewPoints(t *testing.T) {
	outliers, _ := LogIQROutliers([]float64{1, 2, 3}, 1.5)
	assert.Nil(t, outliers)
}

func TestLogIQROutliers_IdenticalValues(t *testing.T) {
	data := []float64{100, 100, 100, 100, 100, 100, 100}
	outliers, _ := LogIQROutliers(data, 1.5)
	assert.Empty(t, outliers)
}

func TestLogIQROutliers_RightSkewed(t *testing.T) {
	// Simulate right-skewed CI durations (log-normal)
	rng := rand.New(rand.NewSource(42)) //nolint:gosec // deterministic seed for reproducible tests
	data := make([]float64, 0, 103)
	for range 100 {
		data = append(data, math.Exp(4.5+0.3*rng.NormFloat64())) // ~90s median
	}
	// Add some genuine outliers
	data = append(data, 500, 600, 700)

	outliers, _ := LogIQROutliers(data, 1.5)
	// Should catch the planted outliers without flagging too many normal values
	assert.GreaterOrEqual(t, len(outliers), 3, "should catch planted outliers")
	assert.LessOrEqual(t, len(outliers), 15, "should not flag too many normal values")
}

func TestMADOutliers_DetectsOutlier(t *testing.T) {
	data := []float64{100, 105, 98, 102, 101, 99, 103, 97, 104, 100,
		101, 98, 102, 100, 103, 99, 101, 100, 102, 98,
		500} // outlier

	outliers := MADOutliers(data, 3.5)
	require.NotEmpty(t, outliers)

	found500 := false
	for _, o := range outliers {
		if o.Value == 500 {
			found500 = true
		}
	}
	assert.True(t, found500)
}

func TestMADOutliers_NoOutliers(t *testing.T) {
	data := []float64{100, 102, 101, 103, 99, 100, 101, 102, 100, 101}
	outliers := MADOutliers(data, 3.5)
	assert.Empty(t, outliers)
}

func TestMADOutliers_TooFewPoints(t *testing.T) {
	outliers := MADOutliers([]float64{1, 2}, 3.5)
	assert.Nil(t, outliers)
}

func TestMADOutliers_IdenticalValues(t *testing.T) {
	data := []float64{100, 100, 100, 100, 100, 100}
	outliers := MADOutliers(data, 3.5)
	assert.Nil(t, outliers)
}

func TestClampOutliers_ClampsExtremeValues(t *testing.T) {
	// 20 normal values around 100, plus one extreme outlier at 2000
	data := []float64{
		100, 105, 98, 102, 101, 99, 103, 97, 104, 100,
		101, 98, 102, 100, 103, 99, 101, 100, 102, 98,
		2000, // extreme outlier
	}
	result := ClampOutliers(data, 4.0)
	require.Len(t, result, len(data))

	// The 2000 should be clamped to the median (~100)
	assert.Less(t, result[20], 200.0, "extreme outlier should be clamped")
	// Normal values should be unchanged
	assert.InDelta(t, data[0], result[0], 0.001)
	assert.InDelta(t, data[5], result[5], 0.001)
}

func TestClampOutliers_PreservesNormalData(t *testing.T) {
	data := []float64{100, 102, 101, 103, 99, 100, 101, 102, 100, 101}
	result := ClampOutliers(data, 4.0)
	assert.Equal(t, data, result, "normal data should be unchanged")
}

func TestClampOutliers_TooFewPoints(t *testing.T) {
	data := []float64{1, 2, 3}
	result := ClampOutliers(data, 4.0)
	assert.Equal(t, data, result, "should return input unchanged")
}

func TestPercentileRank(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	assert.InDelta(t, 0.0, percentileRank(sorted, 1), 0.1)
	assert.InDelta(t, 50.0, percentileRank(sorted, 6), 5)
	assert.InDelta(t, 90.0, percentileRank(sorted, 10), 0.1)
	assert.InDelta(t, 0.0, percentileRank(nil, 5), 0.001)
}
