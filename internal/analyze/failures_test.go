package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

const conclusionFailure = "failure"

func makeFailureDetails() []model.RunDetail {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail

	// 20 runs: 15 success, 3 failure, 2 cancelled
	for i := range 20 {
		start := base.Add(time.Duration(i) * time.Hour)
		conclusion := "success"
		switch i {
		case 5, 10, 15:
			conclusion = conclusionFailure
		case 8, 18:
			conclusion = "cancelled"
		}
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID:           int64(1000 + i),
				WorkflowID:   100,
				WorkflowName: "CI",
				Status:       "completed",
				Conclusion:   conclusion,
				HeadSHA:      "abc123",
				CreatedAt:    start,
				StartedAt:    start,
				UpdatedAt:    start.Add(5 * time.Minute),
			},
			Jobs: []model.Job{
				{
					Name:       "build",
					Status:     "completed",
					Conclusion: conclusion,
				},
			},
		})
	}

	return details
}

func TestFailureAnalyzer_DetectsFailures(t *testing.T) {
	details := makeFailureDetails()

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		AllDetails: details,
	})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	// Should find CI workflow with failure info
	var ciFailure *FailureDetail
	for _, f := range findings {
		d, ok := f.Detail.(FailureDetail)
		if ok && d.Workflow == "CI" {
			ciFailure = &d
			break
		}
	}
	require.NotNil(t, ciFailure, "should detect failures in CI workflow")

	assert.Equal(t, 20, ciFailure.TotalRuns)
	assert.Equal(t, 5, ciFailure.FailureCount) // 3 failure + 2 cancelled
	assert.InDelta(t, 0.25, ciFailure.FailureRate, 0.01)
	assert.Equal(t, 3, ciFailure.ByConclusion[conclusionFailure])
	assert.Equal(t, 2, ciFailure.ByConclusion["cancelled"])
}

func TestFailureAnalyzer_NoFailures(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail
	for i := range 10 {
		start := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				WorkflowID: 100, WorkflowName: "CI",
				Status: "completed", Conclusion: "success",
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(5 * time.Minute),
			},
		})
	}

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{AllDetails: details})
	require.NoError(t, err)
	assert.Empty(t, findings, "should not report workflows with 0% failure rate")
}

func TestFailureAnalyzer_UsesAllDetails(t *testing.T) {
	// AllDetails is empty -> no findings even if Details has data
	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:    makeFailureDetails(),
		AllDetails: nil,
	})
	require.NoError(t, err)
	assert.Empty(t, findings, "should use AllDetails, not Details")
}

func TestFailureDetail_Type(t *testing.T) {
	d := FailureDetail{}
	assert.Equal(t, "failure", d.DetailType())
}
