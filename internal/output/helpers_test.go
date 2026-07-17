package output

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// sixtyPercentFailure builds a failure where the top step accounts for
// exactly 60% of failures (3 of 5) — the documented dominance boundary.
func sixtyPercentFailure() analyze.FailureDetail {
	return analyze.FailureDetail{
		Workflow:     "CI",
		TotalRuns:    20,
		FailureCount: 5,
		FailureRate:  0.25,
		FailingSteps: []analyze.FailingStep{
			{StepName: "integration tests", JobName: "test", Count: 3},
			{StepName: "lint", JobName: "lint", Count: 2},
		},
	}
}

func TestFailingStepHeadline_SixtyPercentIsDominant(t *testing.T) {
	d := sixtyPercentFailure()
	got := failingStepHeadline(&d)
	assert.Contains(t, got, "fails at step",
		"a step causing exactly 60%% of failures is dominant — the table formatter already treats it as such")
}

func TestDominantStepThresholdConsistentAcrossFormatters(t *testing.T) {
	// The dominance rule must be one shared decision, not a per-formatter
	// copy: llm used > 0.6 while table used >= 0.6, so a 3-of-5 failure
	// pattern read as "dominant step" in one format and "spread across
	// steps" in the other.
	d := sixtyPercentFailure()
	result := &analyze.AnalysisResult{
		Findings: []analyze.Finding{{
			Type:     analyze.TypeFailure,
			Severity: analyze.SeverityWarning,
			Detail:   d,
		}},
	}

	var llmBuf bytes.Buffer
	require.NoError(t, LLMFormatter{}.Format(&llmBuf, result))
	assert.Contains(t, llmBuf.String(), "fails at step", "llm formatter")

	var tableBuf bytes.Buffer
	require.NoError(t, TableFormatter{}.Format(&tableBuf, result))
	assert.Contains(t, tableBuf.String(), "fails at:", "table formatter")
}

func TestGroupByType_BucketsEveryKnownType(t *testing.T) {
	// groupByType has no default case: an unbucketed type silently
	// disappears from table/markdown/llm output. AllTypes is the registry
	// this test enforces — add new finding types there.
	findings := make([]analyze.Finding, 0, len(analyze.AllTypes))
	for _, typ := range analyze.AllTypes {
		findings = append(findings, analyze.Finding{Type: typ})
	}

	g := groupByType(findings)
	bucketed := len(g.Summaries) + len(g.Steps) + len(g.Pipelines) +
		len(g.Runners) + len(g.Outliers) + len(g.Changepoints) +
		len(g.Failures) + len(g.Costs)
	assert.Equal(t, len(analyze.AllTypes), bucketed,
		"every type in analyze.AllTypes must land in a groupedFindings bucket")
	assert.GreaterOrEqual(t, len(analyze.AllTypes), 8, "AllTypes must cover the existing types")
}

func TestIsRegressionSlowdown(t *testing.T) {
	tests := []struct {
		name      string
		category  string
		direction string
		want      bool
	}{
		{"regression slowdown", analyze.CategoryRegression, analyze.DirectionSlowdown, true},
		{"regression speedup", analyze.CategoryRegression, analyze.DirectionSpeedup, false},
		{"non-regression slowdown", analyze.CategoryMinor, analyze.DirectionSlowdown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := analyze.ChangePointDetail{Category: tt.category, Direction: tt.direction}
			assert.Equal(t, tt.want, isRegressionSlowdown(&d))
		})
	}
}

func TestDominantFailingStep_NoSteps(t *testing.T) {
	d := analyze.FailureDetail{FailureCount: 3}
	_, dominant := dominantFailingStep(&d)
	assert.False(t, dominant)
}

func TestDominantFailingStep_SingleStepAlwaysDominant(t *testing.T) {
	d := analyze.FailureDetail{
		FailureCount: 4,
		FailingSteps: []analyze.FailingStep{{StepName: "build", Count: 1}},
	}
	top, dominant := dominantFailingStep(&d)
	assert.True(t, dominant)
	assert.Equal(t, "build", top.StepName)
}

func TestDominantFailingStep_BelowThresholdIsDistributed(t *testing.T) {
	d := analyze.FailureDetail{
		FailureCount: 10,
		FailingSteps: []analyze.FailingStep{
			{StepName: "a", Count: 5},
			{StepName: "b", Count: 5},
		},
	}
	_, dominant := dominantFailingStep(&d)
	assert.False(t, dominant, "50%% is below the 60%% dominance share")
}
