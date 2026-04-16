package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func TestPipelineAnalyzer_DetectsStages(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	// Model the Deploy Test Env pattern:
	// Stage 1: 4 parallel test jobs (start together, ~8min)
	// Stage 2: Merge artifacts (serial, ~15s)
	// Stage 3: 2 parallel build jobs (~4min)
	// Stage 4: Deploy (serial, ~10min)
	var details []model.RunDetail
	for i := range 10 {
		start := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(7000 + i), WorkflowID: 700,
				Status: "completed", Conclusion: "success",
				CreatedAt: start, StartedAt: start,
				UpdatedAt: start.Add(23 * time.Minute),
			},
			Jobs: []model.Job{
				{Name: "Unit tests", StartedAt: start, CompletedAt: start.Add(4 * time.Minute)},
				{Name: "Integration tests (1, 3)", StartedAt: start, CompletedAt: start.Add(8 * time.Minute)},
				{Name: "Integration tests (2, 3)", StartedAt: start, CompletedAt: start.Add(8 * time.Minute)},
				{Name: "Integration tests (3, 3)", StartedAt: start, CompletedAt: start.Add(7 * time.Minute)},
				{Name: "Merge artifacts", StartedAt: start.Add(9 * time.Minute), CompletedAt: start.Add(9*time.Minute + 15*time.Second)},
				{Name: "Build lab", StartedAt: start.Add(10 * time.Minute), CompletedAt: start.Add(14 * time.Minute)},
				{Name: "Build admin", StartedAt: start.Add(10 * time.Minute), CompletedAt: start.Add(13 * time.Minute)},
				{Name: "Deploy to Test", StartedAt: start.Add(14 * time.Minute), CompletedAt: start.Add(23 * time.Minute)},
			},
		})
	}

	analyzer := PipelineAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:       details,
		WorkflowNames: map[int64]string{700: "Deploy Test Environment"},
	})
	require.NoError(t, err)
	require.Len(t, findings, 1)

	d, ok := findings[0].Detail.(PipelineDetail)
	require.True(t, ok)

	assert.Equal(t, "Deploy Test Environment", d.Workflow)
	assert.Equal(t, 10, d.TotalRuns)
	assert.Greater(t, d.Parallelism, 0.3, "should detect meaningful parallelism")
	assert.GreaterOrEqual(t, len(d.Stages), 3, "should detect at least 3 stages")

	// Deploy to Test should be the critical path (longest stage)
	assert.Equal(t, "Deploy to Test", d.CriticalPath)
}

func TestPipelineAnalyzer_SingleJobWorkflow(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail
	for i := range 10 {
		start := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(8000 + i), WorkflowID: 800,
				Status: "completed", Conclusion: "success",
				CreatedAt: start, StartedAt: start,
				UpdatedAt: start.Add(5 * time.Minute),
			},
			Jobs: []model.Job{
				{Name: "build", StartedAt: start, CompletedAt: start.Add(5 * time.Minute)},
			},
		})
	}

	analyzer := PipelineAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:       details,
		WorkflowNames: map[int64]string{800: "Simple CI"},
	})
	require.NoError(t, err)
	assert.Empty(t, findings, "single-job workflows should not produce pipeline findings")
}

func TestDetectStages(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	jobs := []model.Job{
		// Stage 1: 3 parallel jobs starting within seconds
		{Name: "test-a", StartedAt: base, CompletedAt: base.Add(5 * time.Minute)},
		{Name: "test-b", StartedAt: base.Add(1 * time.Second), CompletedAt: base.Add(4 * time.Minute)},
		{Name: "test-c", StartedAt: base.Add(2 * time.Second), CompletedAt: base.Add(6 * time.Minute)},
		// Stage 2: starts after stage 1
		{Name: "deploy", StartedAt: base.Add(7 * time.Minute), CompletedAt: base.Add(15 * time.Minute)},
	}

	stages := detectStages(jobs)
	require.Len(t, stages, 2)
	assert.Len(t, stages[0].jobs, 3, "first stage should have 3 parallel jobs")
	assert.Len(t, stages[1].jobs, 1, "second stage should have 1 job")
	assert.Equal(t, "deploy", stages[1].name)
}

func TestPipelineDetail_Type(t *testing.T) {
	d := PipelineDetail{}
	assert.Equal(t, "pipeline", d.DetailType())
}
