package stats

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMedian(t *testing.T) {
	tests := []struct {
		name string
		data []float64
		want float64
	}{
		{"odd count", []float64{1, 3, 5, 7, 9}, 5},
		{"even count", []float64{1, 3, 5, 7}, 4},
		{"single", []float64{42}, 42},
		{"empty", nil, 0},
		{"two values", []float64{10, 20}, 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, Median(tt.data), 0.001)
		})
	}
}

func TestPercentile(t *testing.T) {
	data := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	assert.InDelta(t, 1.0, Percentile(data, 0), 0.001)
	assert.InDelta(t, 5.5, Percentile(data, 50), 0.001)
	assert.InDelta(t, 10.0, Percentile(data, 100), 0.001)
	assert.InDelta(t, 9.55, Percentile(data, 95), 0.1)
}

func TestPercentile_DoesNotModifyInput(t *testing.T) {
	data := []float64{5, 3, 1, 4, 2}
	original := make([]float64, len(data))
	copy(original, data)

	Percentile(data, 50)
	assert.Equal(t, original, data)
}

func TestIQR(t *testing.T) {
	data := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	q1, q3, iqr := IQR(data)

	assert.InDelta(t, 3.25, q1, 0.1)
	assert.InDelta(t, 7.75, q3, 0.1)
	assert.InDelta(t, 4.5, iqr, 0.1)
}

func TestMean(t *testing.T) {
	assert.InDelta(t, 3.0, Mean([]float64{1, 2, 3, 4, 5}), 0.001)
	assert.Equal(t, 0.0, Mean(nil))
}

func TestStddev(t *testing.T) {
	data := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	assert.InDelta(t, 2.0, Stddev(data), 0.2)
	assert.Equal(t, 0.0, Stddev([]float64{5}))
	assert.Equal(t, 0.0, Stddev(nil))
}

func TestStddev_Identical(t *testing.T) {
	data := []float64{5, 5, 5, 5, 5}
	assert.InDelta(t, 0.0, Stddev(data), 0.001)
}

func TestMean_NaN(t *testing.T) {
	// Verify no NaN or Inf
	result := Mean([]float64{1})
	assert.False(t, math.IsNaN(result))
	assert.False(t, math.IsInf(result, 0))
}
