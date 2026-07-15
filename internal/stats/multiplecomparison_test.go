package stats

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBenjaminiHochberg(t *testing.T) {
	// Classic worked example: m=4, sorted ps {0.01, 0.02, 0.03, 0.04}.
	// q_i = min over j>=i of p_(j)*m/j:
	// q_4 = 0.04*4/4 = 0.04; q_3 = min(0.03*4/3, 0.04) = 0.04; q_2 = 0.04; q_1 = 0.04.
	qs := BenjaminiHochberg([]float64{0.02, 0.04, 0.01, 0.03})
	assert.InDelta(t, 0.04, qs[0], 1e-9)
	assert.InDelta(t, 0.04, qs[1], 1e-9)
	assert.InDelta(t, 0.04, qs[2], 1e-9)
	assert.InDelta(t, 0.04, qs[3], 1e-9)

	// One strong signal among noise keeps a small q; borderline raw
	// p-values inflate well past alpha once corrected.
	ps := []float64{0.001, 0.04, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 0.99, 0.3}
	qs = BenjaminiHochberg(ps)
	assert.InDelta(t, 0.01, qs[0], 1e-9, "0.001*10/1")
	assert.Greater(t, qs[1], 0.05, "raw p=0.04 among 10 tests is not significant after correction")

	assert.Empty(t, BenjaminiHochberg(nil))

	// q-values preserve input order and never drop below their raw p.
	for i, q := range qs {
		assert.GreaterOrEqual(t, q, ps[i])
	}
}
