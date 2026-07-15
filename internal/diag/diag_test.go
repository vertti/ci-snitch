package diag

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDiagnosticString(t *testing.T) {
	d := New(Warn, KindNetwork, "run-5", "failed to fetch")
	assert.Equal(t, "[warn] run-5: failed to fetch", d.String())

	noScope := New(Info, KindPreprocess, "", "deduplicated 3 runs")
	assert.Equal(t, "[info] deduplicated 3 runs", noScope.String())
}

func TestDiagnosticString_IncludesCause(t *testing.T) {
	// The wrapped error is the actionable part of a cache/network failure;
	// dropping it left "failed to cache 5 runs" with no why.
	d := Errorf(KindCache, "CI", errors.New("disk full"), "failed to cache %d runs", 5)
	assert.Contains(t, d.String(), "disk full", "the cause must be visible")
}
