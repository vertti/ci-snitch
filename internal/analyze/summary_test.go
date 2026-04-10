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

	// Should have 1 workflow summary + 1 job summary
	require.Len(t, findings, 2)

	var wfFinding, jobFinding Finding
	for _, f := range findings {
		detail := f.Detail.(SummaryDetail)
		switch detail.Subject {
		case "CI":
			wfFinding = f
		case "build":
			jobFinding = f
		}
	}

	// Workflow summary
	wfDetail := wfFinding.Detail.(SummaryDetail)
	assert.Equal(t, 10, wfDetail.TotalRuns)
	assert.Equal(t, 5*time.Minute, wfDetail.Mean)
	assert.Equal(t, 5*time.Minute, wfDetail.Median)
	assert.Equal(t, 5*time.Minute, wfDetail.Min)
	assert.Equal(t, 5*time.Minute, wfDetail.Max)

	// Job summary
	jobDetail := jobFinding.Detail.(SummaryDetail)
	assert.Equal(t, 10, jobDetail.TotalRuns)
	assert.Equal(t, 3*time.Minute, jobDetail.Mean)
}

func TestSummaryAnalyzer_VariedDurations(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	durations := []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		3 * time.Minute,
		4 * time.Minute,
		10 * time.Minute, // outlier
	}

	details := make([]model.RunDetail, len(durations))
	for i, dur := range durations {
		start := base.Add(time.Duration(i) * time.Hour)
		details[i] = model.RunDetail{
			Run: model.WorkflowRun{
				WorkflowID:   100,
				WorkflowName: "CI",
				StartedAt:    start,
				UpdatedAt:    start.Add(dur),
			},
		}
	}

	analyzer := SummaryAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)

	var wfDetail SummaryDetail
	for _, f := range findings {
		d := f.Detail.(SummaryDetail)
		if d.Subject == "CI" {
			wfDetail = d
		}
	}

	assert.Equal(t, 5, wfDetail.TotalRuns)
	assert.Equal(t, 4*time.Minute, wfDetail.Mean)
	assert.Equal(t, 3*time.Minute, wfDetail.Median)
	assert.Equal(t, 1*time.Minute, wfDetail.Min)
	assert.Equal(t, 10*time.Minute, wfDetail.Max)
	assert.Greater(t, wfDetail.P95, wfDetail.Median)
}

func TestSummaryAnalyzer_EmptyInput(t *testing.T) {
	analyzer := SummaryAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{})
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestSummaryDetail_Type(t *testing.T) {
	d := SummaryDetail{}
	assert.Equal(t, "summary", d.DetailType())
}
