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
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:       details,
		WorkflowNames: map[int64]string{100: "CI", 200: "Deploy"},
	})
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

func TestChangePointAnalyzer_Persistence_Sustained(t *testing.T) {
	// 20 runs at 5min, then 30 runs at 8min — clear persistent shift.
	durations := make([]time.Duration, 50)
	for i := range 20 {
		durations[i] = 5*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 20; i < 50; i++ {
		durations[i] = 8*time.Minute + time.Duration(i%3)*time.Second
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	detail, ok := findings[0].Detail.(ChangePointDetail)
	require.True(t, ok)
	assert.Equal(t, "persistent", detail.Persistence)
	assert.Greater(t, detail.PostChangeRuns, 20)
	assert.Greater(t, detail.PostChangeCV, 0.0)
}

func TestChangePointAnalyzer_Persistence_Transient(t *testing.T) {
	// 20 runs at 5min, then 10 at 8min, then 20 back at 5min — transient spike.
	durations := make([]time.Duration, 50)
	for i := range 20 {
		durations[i] = 5*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 20; i < 30; i++ {
		durations[i] = 8*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 30; i < 50; i++ {
		durations[i] = 5*time.Minute + time.Duration(i%3)*time.Second
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)

	// The first change point (slowdown) should be marked transient since it reverts.
	var slowdown *ChangePointDetail
	for _, f := range findings {
		d, ok := f.Detail.(ChangePointDetail)
		require.True(t, ok)
		if d.Direction == "slowdown" {
			slowdown = &d
			break
		}
	}
	require.NotNil(t, slowdown, "should detect slowdown")
	assert.Equal(t, "transient", slowdown.Persistence)
}

func TestChangePointAnalyzer_Persistence_Inconclusive(t *testing.T) {
	// 15 runs at 5min, then only 5 at 8min — too few post-change runs to classify.
	durations := make([]time.Duration, 20)
	for i := range 15 {
		durations[i] = 5*time.Minute + time.Duration(i%3)*time.Second
	}
	for i := 15; i < 20; i++ {
		durations[i] = 8*time.Minute + time.Duration(i%3)*time.Second
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: makeTimedDetails(durations)})
	require.NoError(t, err)

	// With only 5 post-change runs, should be inconclusive.
	for _, f := range findings {
		detail, ok := f.Detail.(ChangePointDetail)
		require.True(t, ok)
		if detail.Direction == "slowdown" {
			assert.Equal(t, "inconclusive", detail.Persistence)
			assert.Equal(t, 5, detail.PostChangeRuns)
		}
	}
}

func TestChangePointDetail_Type(t *testing.T) {
	d := ChangePointDetail{}
	assert.Equal(t, "changepoint", d.DetailType())
}
