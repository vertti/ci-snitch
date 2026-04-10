package stats

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCUSUMDetect_FlatThenJump(t *testing.T) {
	// 20 points at ~100, then 20 points at ~150
	data := make([]float64, 40)
	for i := range 20 {
		data[i] = 100 + float64(i%3) // slight variation
	}
	for i := 20; i < 40; i++ {
		data[i] = 150 + float64(i%3)
	}

	points := CUSUMDetect(data, 4.0, 5)
	require.NotEmpty(t, points, "should detect the jump")

	cp := points[0]
	assert.Equal(t, "slowdown", cp.Direction)
	assert.Greater(t, cp.PctChange, 30.0)
	assert.InDelta(t, 100, cp.BeforeMean, 10)
	assert.InDelta(t, 150, cp.AfterMean, 10)
	// Change point should be near index 20
	assert.InDelta(t, 20, cp.Index, 5)
}

func TestCUSUMDetect_StepDown(t *testing.T) {
	// Speedup: 20 points at ~200, then 20 points at ~120
	data := make([]float64, 40)
	for i := range 20 {
		data[i] = 200 + float64(i%3)
	}
	for i := 20; i < 40; i++ {
		data[i] = 120 + float64(i%3)
	}

	points := CUSUMDetect(data, 4.0, 5)
	require.NotEmpty(t, points)

	cp := points[0]
	assert.Equal(t, "speedup", cp.Direction)
	assert.Less(t, cp.PctChange, -30.0)
}

func TestCUSUMDetect_NoChange(t *testing.T) {
	data := make([]float64, 40)
	for i := range data {
		data[i] = 100 + float64(i%5) // slight noise
	}

	points := CUSUMDetect(data, 4.0, 5)
	assert.Empty(t, points, "should not detect any change in stable data")
}

func TestCUSUMDetect_MultipleChangePoints(t *testing.T) {
	// 100 → 150 → 100
	data := make([]float64, 60)
	for i := range 20 {
		data[i] = 100 + float64(i%3)
	}
	for i := 20; i < 40; i++ {
		data[i] = 150 + float64(i%3)
	}
	for i := 40; i < 60; i++ {
		data[i] = 100 + float64(i%3)
	}

	points := CUSUMDetect(data, 4.0, 5)
	assert.GreaterOrEqual(t, len(points), 2, "should detect both changes")

	if len(points) >= 2 {
		assert.Equal(t, "slowdown", points[0].Direction)
		assert.Equal(t, "speedup", points[1].Direction)
	}
}

func TestCUSUMDetect_TooFewPoints(t *testing.T) {
	points := CUSUMDetect([]float64{1, 2, 3}, 4.0, 5)
	assert.Nil(t, points)
}

func TestCUSUMDetect_IdenticalValues(t *testing.T) {
	data := make([]float64, 30)
	for i := range data {
		data[i] = 100
	}
	points := CUSUMDetect(data, 4.0, 5)
	assert.Empty(t, points)
}

func TestCUSUMDetect_HighVariance(t *testing.T) {
	// Noisy data with a real shift buried in noise
	data := make([]float64, 60)
	for i := range 30 {
		data[i] = 100 + float64(i%10)*5 // 100-145 range
	}
	for i := 30; i < 60; i++ {
		data[i] = 200 + float64(i%10)*5 // 200-245 range — clear shift
	}

	points := CUSUMDetect(data, 4.0, 5)
	require.NotEmpty(t, points, "should detect large shift even in noisy data")
	assert.Equal(t, "slowdown", points[0].Direction)
}
