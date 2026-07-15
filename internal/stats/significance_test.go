package stats

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Tie/discreteness hygiene for the three p-value regimes.

func TestMannWhitneyU_ExactPathHandlesTies(t *testing.T) {
	// Truth values from full enumeration of value assignments (C(10,5)=252
	// combinations, U recomputed with average-rank ties per assignment).
	// The old rank-position enumeration assumed untied data and reported
	// 0.3095 and 0.0556 — anti-conservative on exactly the tie patterns
	// second-resolution CI durations produce.
	_, p := MannWhitneyU([]float64{5, 5, 5, 7, 7}, []float64{5, 7, 7, 7, 7})
	assert.InDelta(t, 0.5238, p, 0.001)

	_, p = MannWhitneyU([]float64{300, 300, 301, 301, 302}, []float64{301, 302, 302, 303, 303})
	assert.InDelta(t, 0.0794, p, 0.001)
}

func TestMannWhitneyU_PermutationPValueNeverZero(t *testing.T) {
	// 25 vs 5 clearly separated samples take the Monte-Carlo path; no
	// shuffle beats the observed extreme, and count/reps returns exactly 0 —
	// overstating the evidence. The observed arrangement is itself a
	// permutation: p must be positive.
	low := make([]float64, 25)
	high := make([]float64, 5)
	for i := range low {
		low[i] = 100 + float64(i)
	}
	for i := range high {
		high[i] = 900 + float64(i)
	}
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test seeding, not crypto
	_, p := MannWhitneyURand(low, high, rng)
	assert.Positive(t, p, "a Monte-Carlo p-value can never be exactly zero")
	assert.Less(t, p, 0.001)
}

func TestNormalApprox_ContinuityCorrection(t *testing.T) {
	// U is discrete; the uncorrected normal approximation is systematically
	// anti-conservative. The corrected p must exceed the uncorrected formula.
	const n1, n2, u = 25, 25, 200.0
	mu := float64(n1) * float64(n2) / 2
	sigma := math.Sqrt(float64(n1) * float64(n2) * float64(n1+n2+1) / 12)
	uncorrected := 2 * (1 - normalCDF(math.Abs(u-mu)/sigma))

	p := normalApproxPValue(n1, n2, u)
	assert.Greater(t, p, uncorrected)
	assert.InDelta(t, uncorrected, p, 0.002, "correction is small at this scale")
}

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

func TestMannWhitneyU_SmallSampleDifferent(t *testing.T) {
	// 5 vs 5 — clearly different. Normal approximation is unreliable here;
	// permutation test should still detect the difference.
	sample1 := []float64{100, 101, 102, 99, 100}
	sample2 := []float64{150, 148, 152, 149, 151}

	_, p := MannWhitneyU(sample1, sample2)
	assert.Less(t, p, 0.05, "small but clearly separated samples should be significant")
}

func TestMannWhitneyU_SmallSampleSimilar(t *testing.T) {
	// 5 vs 5 — overlapping distributions. Should NOT be significant.
	sample1 := []float64{100, 105, 98, 103, 101}
	sample2 := []float64{102, 99, 104, 100, 103}

	_, p := MannWhitneyU(sample1, sample2)
	assert.Greater(t, p, 0.05, "overlapping small samples should not be significant")
}

func TestMannWhitneyU_AsymmetricSmallSample(t *testing.T) {
	// 20 vs 5 — one side large, one small. Should use permutation for the small side.
	sample1 := []float64{100, 102, 98, 101, 99, 103, 97, 100, 101, 98,
		102, 100, 99, 101, 103, 98, 100, 102, 99, 101}
	sample2 := []float64{150, 148, 152, 149, 151}

	_, p := MannWhitneyU(sample1, sample2)
	assert.Less(t, p, 0.05, "clearly different even with asymmetric sizes")
}

func TestMannWhitneyU_ExactSmallSample(t *testing.T) {
	// For [1,2,3] vs [4,5,6], the exact two-sided p-value is 0.1
	// (U=0, and there are 20 possible arrangements of 3+3, exactly 2 give U<=0).
	// The normal approximation gives ~0.0463 — incorrectly significant!
	// This is the canonical example of the normal approximation failing for small n.
	sample1 := []float64{1, 2, 3}
	sample2 := []float64{4, 5, 6}

	_, p := MannWhitneyU(sample1, sample2)
	assert.InDelta(t, 0.1, p, 0.02, "exact p-value for 3v3 complete separation should be ~0.1")
}

func TestMannWhitneyURand_Deterministic(t *testing.T) {
	// Permutation path (20 vs 5) should produce identical p-values with a seeded RNG.
	sample1 := []float64{100, 102, 98, 101, 99, 103, 97, 100, 101, 98,
		102, 100, 99, 101, 103, 98, 100, 102, 99, 101}
	sample2 := []float64{150, 148, 152, 149, 151}

	rng1 := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic seed for test
	_, p1 := MannWhitneyURand(sample1, sample2, rng1)

	rng2 := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic seed for test
	_, p2 := MannWhitneyURand(sample1, sample2, rng2)

	assert.InDelta(t, p1, p2, 0, "same seed must produce identical p-values")
	assert.Less(t, p1, 0.05, "clearly different distributions should be significant")
}

func TestMannWhitneyU_LargeSampleUsesApproximation(t *testing.T) {
	// Both samples > 20: should still work (uses normal approximation).
	sample1 := make([]float64, 25)
	sample2 := make([]float64, 25)
	for i := range 25 {
		sample1[i] = float64(100 + i%5)
		sample2[i] = float64(200 + i%5)
	}

	_, p := MannWhitneyU(sample1, sample2)
	assert.Less(t, p, 0.001, "large clearly different samples should be very significant")
}
