package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func makeDetails(n int, workflowDur, jobDur time.Duration) []model.RunDetail {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	details := make([]model.RunDetail, n)
	for i := range n {
		start := base.Add(time.Duration(i) * time.Hour)
		details[i] = model.RunDetail{
			Run: model.WorkflowRun{
				ID:           int64(1000 + i),
				WorkflowID:   100,
				WorkflowName: "CI",
				StartedAt:    start,
				UpdatedAt:    start.Add(workflowDur),
			},
			Jobs: []model.Job{
				{
					Name:        "build",
					StartedAt:   start,
					CompletedAt: start.Add(jobDur),
				},
			},
		}
	}
	return details
}

func TestSummaryAnalyzer_BasicStats(t *testing.T) {
	details := makeDetails(10, 5*time.Minute, 3*time.Minute)

	analyzer := SummaryAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)

	// One finding per workflow (jobs nested inside)
	require.Len(t, findings, 1)

	d, ok := findings[0].Detail.(SummaryDetail)
	require.True(t, ok)

	assert.Equal(t, "CI", d.Workflow)
	assert.Equal(t, 10, d.Stats.TotalRuns)
	assert.Equal(t, 5*time.Minute, d.Stats.Mean)
	assert.Equal(t, 5*time.Minute, d.Stats.Median)
	assert.Equal(t, 5*time.Minute, d.Stats.Min)
	assert.Equal(t, 5*time.Minute, d.Stats.Max)
	assert.Equal(t, 50*time.Minute, d.Stats.TotalTime)

	// Jobs nested under workflow
	require.Len(t, d.Jobs, 1)
	assert.Equal(t, "build", d.Jobs[0].Name)
	assert.Equal(t, 10, d.Jobs[0].Stats.TotalRuns)
	assert.Equal(t, 3*time.Minute, d.Jobs[0].Stats.Mean)
}

func TestSummaryAnalyzer_SortedByTotalTime(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	details := []model.RunDetail{
		// "Fast" workflow: 10 runs at 1 min = 10 min total
		{
			Run: model.WorkflowRun{WorkflowID: 1, WorkflowName: "Fast", StartedAt: base, UpdatedAt: base.Add(1 * time.Minute)},
		},
		// "Slow" workflow: 1 run at 30 min = 30 min total
		{
			Run: model.WorkflowRun{WorkflowID: 2, WorkflowName: "Slow", StartedAt: base, UpdatedAt: base.Add(30 * time.Minute)},
		},
	}
	// Add more Fast runs
	for i := 1; i < 10; i++ {
		s := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{WorkflowID: 1, WorkflowName: "Fast", StartedAt: s, UpdatedAt: s.Add(1 * time.Minute)},
		})
	}

	analyzer := SummaryAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	require.Len(t, findings, 2)

	// Slow should be first (30 min > 10 min total)
	first, ok := findings[0].Detail.(SummaryDetail)
	require.True(t, ok)
	assert.Equal(t, "Slow", first.Workflow)
}

func TestSummaryAnalyzer_JobsSortedByMedian(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	details := make([]model.RunDetail, 5)
	for i := range details {
		s := base.Add(time.Duration(i) * time.Hour)
		details[i] = model.RunDetail{
			Run: model.WorkflowRun{WorkflowID: 1, WorkflowName: "CI", StartedAt: s, UpdatedAt: s.Add(10 * time.Minute)},
			Jobs: []model.Job{
				{Name: "fast-job", StartedAt: s, CompletedAt: s.Add(1 * time.Minute)},
				{Name: "slow-job", StartedAt: s, CompletedAt: s.Add(8 * time.Minute)},
				{Name: "mid-job", StartedAt: s, CompletedAt: s.Add(3 * time.Minute)},
			},
		}
	}

	analyzer := SummaryAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)

	d, ok := findings[0].Detail.(SummaryDetail)
	require.True(t, ok)
	require.Len(t, d.Jobs, 3)
	assert.Equal(t, "slow-job", d.Jobs[0].Name)
	assert.Equal(t, "mid-job", d.Jobs[1].Name)
	assert.Equal(t, "fast-job", d.Jobs[2].Name)
}

func TestSummaryAnalyzer_VariedDurations(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	durations := []time.Duration{1 * time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 10 * time.Minute}

	details := make([]model.RunDetail, len(durations))
	for i, dur := range durations {
		start := base.Add(time.Duration(i) * time.Hour)
		details[i] = model.RunDetail{
			Run: model.WorkflowRun{WorkflowID: 100, WorkflowName: "CI", StartedAt: start, UpdatedAt: start.Add(dur)},
		}
	}

	analyzer := SummaryAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)

	d, ok := findings[0].Detail.(SummaryDetail)
	require.True(t, ok)
	assert.Equal(t, 5, d.Stats.TotalRuns)
	assert.Equal(t, 4*time.Minute, d.Stats.Mean)
	assert.Equal(t, 3*time.Minute, d.Stats.Median)
	assert.Equal(t, 1*time.Minute, d.Stats.Min)
	assert.Equal(t, 10*time.Minute, d.Stats.Max)
	assert.Greater(t, d.Stats.P95, d.Stats.Median)
}

func TestSummaryAnalyzer_EmptyInput(t *testing.T) {
	analyzer := SummaryAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{})
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestSummaryAnalyzer_VolatilityScoring(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		durations []time.Duration
		wantLabel string
	}{
		{
			name:      "stable - all same duration",
			durations: repeat(20, 5*time.Minute),
			wantLabel: "stable",
		},
		{
			name: "variable - moderate spread",
			durations: append(
				repeat(15, 5*time.Minute),
				repeat(5, 8*time.Minute)...,
			),
			wantLabel: "variable",
		},
		{
			name: "spiky - large tail",
			durations: append(
				repeat(18, 5*time.Minute),
				repeat(2, 12*time.Minute)...,
			),
			wantLabel: "spiky",
		},
		{
			name: "volatile - extreme spread",
			durations: append(
				repeat(15, 5*time.Minute),
				repeat(5, 30*time.Minute)...,
			),
			wantLabel: "volatile",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			details := make([]model.RunDetail, len(tt.durations))
			for i, dur := range tt.durations {
				start := base.Add(time.Duration(i) * time.Hour)
				details[i] = model.RunDetail{
					Run: model.WorkflowRun{
						WorkflowID: 100, WorkflowName: "CI",
						StartedAt: start, UpdatedAt: start.Add(dur),
					},
				}
			}

			analyzer := SummaryAnalyzer{}
			findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
			require.NoError(t, err)
			require.NotEmpty(t, findings)

			d, ok := findings[0].Detail.(SummaryDetail)
			require.True(t, ok)
			assert.Equal(t, tt.wantLabel, d.Stats.VolatilityLabel,
				"volatility=%.2f", d.Stats.Volatility)
		})
	}
}

func repeat(n int, d time.Duration) []time.Duration {
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = d
	}
	return out
}

func TestSummaryDetail_Type(t *testing.T) {
	d := SummaryDetail{}
	assert.Equal(t, "summary", d.DetailType())
}
