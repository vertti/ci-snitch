package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

const defaultTestDur = 5 * time.Minute

func makeOutlierDetails(n int) []model.RunDetail {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	details := make([]model.RunDetail, n)
	for i := range n {
		start := base.Add(time.Duration(i) * time.Hour)
		details[i] = model.RunDetail{
			Run: model.WorkflowRun{
				ID:           int64(1000 + i),
				WorkflowID:   100,
				WorkflowName: "CI",
				HeadSHA:      "abc123",
				StartedAt:    start,
				UpdatedAt:    start.Add(defaultTestDur),
			},
			Jobs: []model.Job{
				{
					Name:        "build",
					StartedAt:   start,
					CompletedAt: start.Add(defaultTestDur / 2),
				},
			},
		}
	}
	return details
}

func TestOutlierAnalyzer_DetectsSlowRun(t *testing.T) {
	details := makeOutlierDetails(20)
	// Make one run much slower
	slow := &details[15]
	slow.Run.UpdatedAt = slow.Run.StartedAt.Add(30 * time.Minute)
	slow.Jobs[0].CompletedAt = slow.Jobs[0].StartedAt.Add(15 * time.Minute)

	analyzer := OutlierAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	// Should find the slow run as an outlier
	foundSlow := false
	for _, f := range findings {
		detail, ok := f.Detail.(OutlierDetail)
		require.True(t, ok)
		if detail.RunID == slow.Run.ID {
			foundSlow = true
			assert.Greater(t, detail.Percentile, 90.0)
		}
	}
	assert.True(t, foundSlow, "should detect the 30min run as outlier among 5min runs")
}

func TestOutlierAnalyzer_NoOutliersWhenUniform(t *testing.T) {
	details := makeOutlierDetails(20)

	analyzer := OutlierAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestOutlierAnalyzer_TooFewRuns(t *testing.T) {
	details := makeOutlierDetails(3)

	analyzer := OutlierAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestOutlierAnalyzer_MADMethod(t *testing.T) {
	details := makeOutlierDetails(20)
	// Add slight variation so MAD isn't zero
	for i := range details {
		jitter := time.Duration(i*10) * time.Second
		details[i].Run.UpdatedAt = details[i].Run.UpdatedAt.Add(jitter)
		details[i].Jobs[0].CompletedAt = details[i].Jobs[0].CompletedAt.Add(jitter)
	}
	slow := &details[10]
	slow.Run.UpdatedAt = slow.Run.StartedAt.Add(60 * time.Minute)
	slow.Jobs[0].CompletedAt = slow.Jobs[0].StartedAt.Add(30 * time.Minute)

	analyzer := OutlierAnalyzer{Method: "mad"}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	require.NotEmpty(t, findings)
}

func TestOutlierAnalyzer_DetectsSlowJob(t *testing.T) {
	details := makeOutlierDetails(20)
	// Make one job much slower (but not the workflow)
	slow := &details[15]
	slow.Jobs[0].CompletedAt = slow.Jobs[0].StartedAt.Add(20 * time.Minute)

	analyzer := OutlierAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)

	foundSlowJob := false
	for _, f := range findings {
		detail, ok := f.Detail.(OutlierDetail)
		require.True(t, ok)
		if detail.JobName == "build" && detail.RunID == slow.Run.ID {
			foundSlowJob = true
		}
	}
	assert.True(t, foundSlowJob, "should detect the slow build job")
}

func TestOutlierAnalyzer_MinPercentileFilter(t *testing.T) {
	details := makeOutlierDetails(100)
	// Add several outliers of varying severity
	for _, idx := range []int{90, 92, 95, 98} {
		details[idx].Run.UpdatedAt = details[idx].Run.StartedAt.Add(time.Duration(30+idx) * time.Minute)
		details[idx].Jobs[0].CompletedAt = details[idx].Jobs[0].StartedAt.Add(time.Duration(15+idx) * time.Minute)
	}

	// Default (MinPercentile=95): should filter out low-percentile outliers
	analyzer := OutlierAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)

	for _, f := range findings {
		detail, ok := f.Detail.(OutlierDetail)
		require.True(t, ok)
		assert.GreaterOrEqual(t, detail.Percentile, 95.0,
			"default should not report outliers below p95")
	}
}

func TestOutlierDetail_Type(t *testing.T) {
	d := OutlierDetail{}
	assert.Equal(t, "outlier", d.DetailType())
}

func TestSeverityFromPercentile(t *testing.T) {
	assert.Equal(t, "critical", severityFromPercentile(99.5))
	assert.Equal(t, "warning", severityFromPercentile(96))
	assert.Equal(t, "info", severityFromPercentile(80))
}
