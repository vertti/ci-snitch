package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func makeTimedDetails(durations []time.Duration) []model.RunDetail {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	details := make([]model.RunDetail, len(durations))
	for i, dur := range durations {
		start := base.Add(time.Duration(i) * time.Hour)
		details[i] = model.RunDetail{
			Run: model.WorkflowRun{
				ID:           int64(1000 + i),
				WorkflowID:   100,
				WorkflowName: "CI",
				HeadSHA:      "abc12345",
				CreatedAt:    start,
				StartedAt:    start,
				UpdatedAt:    start.Add(dur),
			},
			Jobs: []model.Job{
				{
					Name:        "build",
					StartedAt:   start,
					CompletedAt: start.Add(dur),
				},
			},
		}
	}
	return details
}

func TestChangePointAnalyzer_DetectsSlowdown(t *testing.T) {
	// 20 runs at 5min, then 20 runs at 8min
	durations := make([]time.Duration, 40)
	for i := range 20 {
		durations[i] = 5*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 20; i < 40; i++ {
		durations[i] = 8*time.Minute + time.Duration(i%3)*time.Second
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	f := findings[0]
	detail, ok := f.Detail.(ChangePointDetail)
	require.True(t, ok)
	assert.Equal(t, "slowdown", detail.Direction)
	assert.Greater(t, detail.PctChange, 40.0)
	assert.Equal(t, "build", detail.JobName)
	assert.NotEmpty(t, detail.CommitSHA)
}

func TestChangePointAnalyzer_DetectsSpeedup(t *testing.T) {
	durations := make([]time.Duration, 40)
	for i := range 20 {
		durations[i] = 10*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 20; i < 40; i++ {
		durations[i] = 6*time.Minute + time.Duration(i%3)*time.Second
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	found := false
	for _, f := range findings {
		detail, ok := f.Detail.(ChangePointDetail)
		require.True(t, ok)
		if detail.Direction == "speedup" {
			found = true
			assert.Less(t, detail.PctChange, -30.0)
		}
	}
	assert.True(t, found, "should detect speedup")
}

func TestChangePointAnalyzer_NoChange(t *testing.T) {
	durations := make([]time.Duration, 30)
	for i := range durations {
		durations[i] = 5*time.Minute + time.Duration(i%5)*time.Second
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestChangePointAnalyzer_TooFewRuns(t *testing.T) {
	durations := []time.Duration{5 * time.Minute, 6 * time.Minute, 5 * time.Minute}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestChangePointAnalyzer_MultipleChangePoints(t *testing.T) {
	// 5min → 8min → 5min
	durations := make([]time.Duration, 60)
	for i := range 20 {
		durations[i] = 5*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 20; i < 40; i++ {
		durations[i] = 8*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 40; i < 60; i++ {
		durations[i] = 5*time.Minute + time.Duration(i%3)*time.Second
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(findings), 2, "should detect slowdown and speedup")
}

func TestChangePointAnalyzer_SignificanceTest(t *testing.T) {
	// Clear shift should have low p-value
	durations := make([]time.Duration, 40)
	for i := range 20 {
		durations[i] = 5*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 20; i < 40; i++ {
		durations[i] = 8*time.Minute + time.Duration(i%3)*time.Second
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	detail, ok := findings[0].Detail.(ChangePointDetail)
	require.True(t, ok)
	assert.Less(t, detail.PValue, 0.05, "clear shift should be statistically significant")
}

func TestChangePointDetail_Type(t *testing.T) {
	d := ChangePointDetail{}
	assert.Equal(t, "changepoint", d.DetailType())
}
