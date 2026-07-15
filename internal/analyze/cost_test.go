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
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:       details,
		WorkflowNames: map[int64]string{100: "CI"},
	})
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

func TestCostAnalyzer_IncludesNonSuccessRuns(t *testing.T) {
	// GitHub bills failed and cancelled runs too, so cost must be computed
	// from AllDetails, not the success-only Details.
	details := makeCostDetails() // 10 success runs
	base := time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)
	failed := model.RunDetail{
		Run: model.WorkflowRun{
			ID: 2000, WorkflowID: 100, WorkflowName: "CI",
			Status: "completed", Conclusion: "failure",
			CreatedAt: base, StartedAt: base, UpdatedAt: base.Add(10 * time.Minute),
		},
		Jobs: []model.Job{{
			Name: "build", StartedAt: base, CompletedAt: base.Add(3*time.Minute + 30*time.Second),
			Labels: []string{"ubuntu-latest"},
		}},
	}
	all := append(append([]model.RunDetail{}, details...), failed)

	analyzer := CostAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:       details,
		AllDetails:    all,
		WorkflowNames: map[int64]string{100: "CI"},
	})
	require.NoError(t, err)

	var ciCost *CostDetail
	for _, f := range findings {
		if d, ok := f.Detail.(CostDetail); ok && d.Workflow == "CI" {
			ciCost = &d
			break
		}
	}
	require.NotNil(t, ciCost)

	// 340 from the 10 success runs (see TestCostAnalyzer_ComputesCost)
	// + failed run's build job: 3m30s -> 4 billable minutes * 1x = 4
	assert.InDelta(t, 344, ciCost.BillableMinutes, 1, "failed runs are billed and must be counted")
	assert.Equal(t, 11, ciCost.TotalRuns, "failed runs count toward total")
}

func TestCostAnalyzer_MultiplierLookedUpPerRun(t *testing.T) {
	// A job migrated mid-window (ubuntu-latest -> self-hosted) must have
	// each run priced by its own labels — not all runs priced by whichever
	// variant happened to be seen first.
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	mkRun := func(id int64, offset time.Duration, labels []string) model.RunDetail {
		start := base.Add(offset)
		return model.RunDetail{
			Run: model.WorkflowRun{
				ID: id, WorkflowID: 100, WorkflowName: "CI",
				Status: "completed", Conclusion: "success",
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(10 * time.Minute),
			},
			Jobs: []model.Job{{
				Name: "build", StartedAt: start, CompletedAt: start.Add(3*time.Minute + 30*time.Second),
				Labels: labels,
			}},
		}
	}

	for name, details := range map[string][]model.RunDetail{
		"hosted first": {
			mkRun(1, 0, []string{"ubuntu-latest"}),
			mkRun(2, time.Hour, []string{"self-hosted", "linux"}),
		},
		"self-hosted first": {
			mkRun(2, time.Hour, []string{"self-hosted", "linux"}),
			mkRun(1, 0, []string{"ubuntu-latest"}),
		},
	} {
		t.Run(name, func(t *testing.T) {
			analyzer := CostAnalyzer{}
			findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
				Details:       details,
				WorkflowNames: map[int64]string{100: "CI"},
			})
			require.NoError(t, err)

			var ciCost *CostDetail
			for _, f := range findings {
				if d, ok := f.Detail.(CostDetail); ok && d.Workflow == "CI" {
					ciCost = &d
					break
				}
			}
			require.NotNil(t, ciCost)
			// One hosted run: 3m30s -> 4 billable minutes at 1x.
			// One self-hosted run: 4 free minutes.
			assert.InDelta(t, 4, ciCost.BillableMinutes, 0.001, "only the hosted run is billed")
			assert.InDelta(t, 4, ciCost.SelfHostedMinutes, 0.001, "the self-hosted run's minutes are free")
		})
	}
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
