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

func TestChangePointAnalyzer_SameJobNameDifferentWorkflows(t *testing.T) {
	// Two workflows both have a job named "test".
	// Workflow A: stable at 5min. Workflow B: shifts from 3min to 6min.
	// Bug: if keyed by job name alone, the distributions get mixed
	// and the change point may not be detected (or is attributed wrong).
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail

	for i := range 40 {
		start := base.Add(time.Duration(i) * time.Hour)

		// Workflow A "CI": job "test" always ~5min
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID:           int64(1000 + i),
				WorkflowID:   100,
				WorkflowName: "CI",
				HeadSHA:      "aaa11111",
				CreatedAt:    start,
				StartedAt:    start,
				UpdatedAt:    start.Add(5*time.Minute + time.Duration(i%3)*time.Second),
			},
			Jobs: []model.Job{
				{
					Name:        "test",
					StartedAt:   start,
					CompletedAt: start.Add(5*time.Minute + time.Duration(i%3)*time.Second),
				},
			},
		})

		// Workflow B "Deploy": job "test" shifts from 3min to 6min at i=20
		dur := 3*time.Minute + time.Duration(i%3)*time.Second
		if i >= 20 {
			dur = 6*time.Minute + time.Duration(i%3)*time.Second
		}
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID:           int64(2000 + i),
				WorkflowID:   200,
				WorkflowName: "Deploy",
				HeadSHA:      "bbb22222",
				CreatedAt:    start.Add(30 * time.Minute),
				StartedAt:    start.Add(30 * time.Minute),
				UpdatedAt:    start.Add(30*time.Minute + dur),
			},
			Jobs: []model.Job{
				{
					Name:        "test",
					StartedAt:   start.Add(30 * time.Minute),
					CompletedAt: start.Add(30*time.Minute + dur),
				},
			},
		})
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)

	// Should detect a change point in Deploy's "test" job, not in CI's "test" job.
	// With (workflow, job) keying, these are separate time series.
	var deployFindings, ciFindings []ChangePointDetail
	for _, f := range findings {
		detail, ok := f.Detail.(ChangePointDetail)
		require.True(t, ok)
		switch detail.WorkflowName {
		case "Deploy":
			deployFindings = append(deployFindings, detail)
		case "CI":
			ciFindings = append(ciFindings, detail)
		}
	}

	assert.NotEmpty(t, deployFindings, "should detect change point in Deploy workflow's test job")
	assert.Empty(t, ciFindings, "should NOT detect change point in CI workflow's stable test job")
}

func TestChangePointDetail_Type(t *testing.T) {
	d := ChangePointDetail{}
	assert.Equal(t, "changepoint", d.DetailType())
}
