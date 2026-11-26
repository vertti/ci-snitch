package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func makeStepDetails() []model.RunDetail {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail

	for i := range 10 {
		start := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID:         int64(2000 + i),
				WorkflowID: 200,
				StartedAt:  start,
				UpdatedAt:  start.Add(10 * time.Minute),
			},
			Jobs: []model.Job{
				{
					Name:        "build",
					StartedAt:   start,
					CompletedAt: start.Add(8 * time.Minute),
					Steps: []model.Step{
						{Name: "Checkout", StartedAt: start, CompletedAt: start.Add(5 * time.Second)},
						{Name: "Setup Go", StartedAt: start.Add(5 * time.Second), CompletedAt: start.Add(30 * time.Second)},
						{Name: "Run tests", StartedAt: start.Add(30 * time.Second), CompletedAt: start.Add(6 * time.Minute)},
						{Name: "Build binary", StartedAt: start.Add(6 * time.Minute), CompletedAt: start.Add(8 * time.Minute)},
					},
				},
			},
		})
	}
	return details
}

func TestStepAnalyzer_BasicOutput(t *testing.T) {
	details := makeStepDetails()
	analyzer := StepAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	d, ok := findings[0].Detail.(StepTimingDetail)
	require.True(t, ok)
	assert.Equal(t, "build", d.JobName)
	assert.Equal(t, 10, d.TotalRuns)

	// Top 3 steps by duration should be: Run tests, Build binary, Setup Go
	require.Len(t, d.Steps, 3)
	assert.Equal(t, "Run tests", d.Steps[0].Name)
	assert.Equal(t, "Build binary", d.Steps[1].Name)
	assert.Equal(t, "Setup Go", d.Steps[2].Name)

	// Run tests takes ~5.5min of 8min job ≈ 69%
	assert.Greater(t, d.Steps[0].PctOfJob, 50.0)
}

func TestStepAnalyzer_TooFewRuns(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	details := []model.RunDetail{
		{
			Run: model.WorkflowRun{WorkflowID: 1, StartedAt: base, UpdatedAt: base.Add(5 * time.Minute)},
			Jobs: []model.Job{
				{
					Name:        "build",
					StartedAt:   base,
					CompletedAt: base.Add(5 * time.Minute),
					Steps: []model.Step{
						{Name: "test", StartedAt: base, CompletedAt: base.Add(4 * time.Minute)},
					},
				},
			},
		},
	}
	analyzer := StepAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	assert.Empty(t, findings, "should not report steps with too few runs")
}

func TestStepAnalyzer_EmptyInput(t *testing.T) {
	analyzer := StepAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{})
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestStepTimingDetail_Type(t *testing.T) {
	d := StepTimingDetail{}
	assert.Equal(t, "steps", d.DetailType())
}

func TestStepAnalyzer_JobMedianIgnoresFetchOrder(t *testing.T) {
	// "build" lasts 10s, 100s, 20s in fetch order: median 20s, not the middle
	// element. "lint" always lasts 30s, so it is the slower job.
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail
	for i, buildSeconds := range []int{10, 100, 20} {
		start := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{ID: int64(3000 + i), WorkflowID: 300, StartedAt: start},
			Jobs: []model.Job{
				{
					Name: "build", StartedAt: start, CompletedAt: start.Add(time.Duration(buildSeconds) * time.Second),
					Steps: []model.Step{{Name: "Compile", StartedAt: start, CompletedAt: start.Add(5 * time.Second)}},
				},
				{
					Name: "lint", StartedAt: start, CompletedAt: start.Add(30 * time.Second),
					Steps: []model.Step{{Name: "Lint", StartedAt: start, CompletedAt: start.Add(15 * time.Second)}},
				},
			},
		})
	}

	findings, err := StepAnalyzer{}.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	require.Len(t, findings, 2)

	byJob := make(map[string]StepTimingDetail)
	var order []string
	for _, f := range findings {
		d, ok := f.Detail.(StepTimingDetail)
		require.True(t, ok)
		byJob[d.JobName] = d
		order = append(order, d.JobName)
	}
	assert.Equal(t, []string{"lint", "build"}, order, "jobs are ranked by median duration")
	require.Len(t, byJob["build"].Steps, 1)
	assert.InDelta(t, 25.0, byJob["build"].Steps[0].PctOfJob, 0.01, "5s of a 20s median job")
}
