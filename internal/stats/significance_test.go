package stats

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMannWhitneyU_DifferentDistributions(t *testing.T) {
	sample1 := []float64{100, 102, 98, 101, 99, 103, 97, 100, 101, 98,
		102, 100, 99, 101, 103, 98, 100, 102, 99, 101}
	sample2 := []float64{150, 148, 152, 149, 151, 147, 153, 150, 148, 152,
		149, 151, 147, 150, 152, 148, 151, 149, 153, 150}

	_, p := MannWhitneyU(sample1, sample2)
	assert.Less(t, p, 0.05, "clearly different distributions should have p < 0.05")
}

func TestMannWhitneyU_SameDistribution(t *testing.T) {
	sample1 := []float64{100, 102, 98, 101, 99, 103, 97, 100, 101, 98,
		102, 100, 99, 101, 103, 98, 100, 102, 99, 101}
	sample2 := []float64{101, 99, 100, 102, 98, 100, 103, 99, 101, 100,
		98, 102, 100, 101, 99, 103, 100, 98, 101, 102}

	_, p := MannWhitneyU(sample1, sample2)
	assert.Greater(t, p, 0.05, "similar distributions should have p > 0.05")
}

func TestMannWhitneyU_EmptySample(t *testing.T) {
	_, p := MannWhitneyU(nil, []float64{1, 2, 3})
	assert.InDelta(t, 1.0, p, 0.001)

	_, p = MannWhitneyU([]float64{1, 2, 3}, nil)
	assert.InDelta(t, 1.0, p, 0.001)
}

func TestMannWhitneyU_IdenticalValues(t *testing.T) {
	sample1 := []float64{5, 5, 5, 5, 5}
	sample2 := []float64{5, 5, 5, 5, 5}

	_, p := MannWhitneyU(sample1, sample2)
	assert.Greater(t, p, 0.05)
}
