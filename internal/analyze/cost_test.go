package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func makeCostDetails() []model.RunDetail {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail

	for i := range 10 {
		start := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(1000 + i), WorkflowID: 100, WorkflowName: "CI",
				Status: "completed", Conclusion: "success",
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(10 * time.Minute),
			},
			Jobs: []model.Job{
				{
					Name: "build", StartedAt: start, CompletedAt: start.Add(3*time.Minute + 30*time.Second),
					Labels: []string{"ubuntu-latest"},
				},
				{
					Name: "test-mac", StartedAt: start, CompletedAt: start.Add(2*time.Minute + 15*time.Second),
					Labels: []string{"macos-latest"},
				},
			},
		})
	}

	return details
}

func TestCostAnalyzer_ComputesCost(t *testing.T) {
	details := makeCostDetails()

	analyzer := CostAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{Details: details})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	// Should have one finding for "CI" workflow
	var ciCost *CostDetail
	for _, f := range findings {
		d, ok := f.Detail.(CostDetail)
		if ok && d.Workflow == "CI" {
			ciCost = &d
			break
		}
	}
	require.NotNil(t, ciCost)

	// build: 3m30s -> 4 billable mins * 1x * 10 runs = 40
	// test-mac: 2m15s -> 3 billable mins * 10x * 10 runs = 300
	// Total: 340 billable minutes
	assert.InDelta(t, 340, ciCost.BillableMinutes, 1)
	assert.Equal(t, 10, ciCost.TotalRuns)
	assert.Greater(t, ciCost.DailyRate, 0.0)
}

func TestCostAnalyzer_Empty(t *testing.T) {
	analyzer := CostAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{})
	require.NoError(t, err)
	assert.Empty(t, findings)
}

func TestCostDetail_Type(t *testing.T) {
	d := CostDetail{}
	assert.Equal(t, "cost", d.DetailType())
}
