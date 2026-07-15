package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

const (
	conclusionFailure = "failure"
	conclusionSuccess = "success"
)

func makeFailureDetails() []model.RunDetail {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail

	// 20 runs: 15 success, 3 failure, 2 cancelled
	for i := range 20 {
		start := base.Add(time.Duration(i) * time.Hour)
		conclusion := conclusionSuccess
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

func TestFailureAnalyzer_TrendComparesDisjointPeriods(t *testing.T) {
	// 14 days of runs: prior week 3/25 failures (12%), recent week 5/25
	// (20%). Comparing recent against the OVERALL rate dilutes the signal
	// (0.20 vs 0.16 = +0.04, under the 0.05 trigger); comparing against the
	// prior period (+0.08) correctly reports worsening.
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail
	mk := func(id int, day float64, conclusion string) model.RunDetail {
		start := base.Add(time.Duration(day * 24 * float64(time.Hour)))
		return model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(id), WorkflowID: 100, WorkflowName: "CI",
				Status: "completed", Conclusion: conclusion, HeadSHA: "abc123",
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(5 * time.Minute),
			},
			Jobs: []model.Job{{Name: "build", Status: "completed", Conclusion: conclusion}},
		}
	}
	for i := range 25 { // prior week: 3 failures
		c := conclusionSuccess
		if i < 3 {
			c = conclusionFailure
		}
		details = append(details, mk(1000+i, float64(i)*0.25, c))
	}
	for i := range 25 { // recent week: 5 failures
		c := conclusionSuccess
		if i < 5 {
			c = conclusionFailure
		}
		details = append(details, mk(2000+i, 7.5+float64(i)*0.25, c))
	}

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		AllDetails:    details,
		WorkflowNames: map[int64]string{100: "CI"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)
	assert.Equal(t, FailureTrendWorsening, d.Trend,
		"a 12%%→20%% week-over-week rise must not be diluted to 'stable' by including the recent week in the baseline")
}

func TestFailureAnalyzer_SkippedRunsExcludedFromRate(t *testing.T) {
	// Path-filtered workflows produce lots of skipped runs; counting them in
	// the denominator deflates the failure rate of the runs that actually ran.
	details := makeConclusionDetails(map[string]int{
		conclusionSuccess: 4, "skipped": 4, conclusionFailure: 2,
	})

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		AllDetails:    details,
		WorkflowNames: map[int64]string{100: "CI"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)
	assert.Equal(t, 6, d.TotalRuns, "skipped runs never executed and don't belong in the denominator")
	assert.InDelta(t, 2.0/6.0, d.FailureRate, 0.001)
}

func TestFailureAnalyzer_NeutralIsNotAFailure(t *testing.T) {
	// "neutral" is a deliberate non-failing conclusion (checks that opt out);
	// treating it as a failure invents a 50% failure rate here.
	details := makeConclusionDetails(map[string]int{
		conclusionSuccess: 5, "neutral": 5,
	})

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		AllDetails:    details,
		WorkflowNames: map[int64]string{100: "CI"},
	})
	require.NoError(t, err)
	assert.Empty(t, findings, "neutral conclusions must not produce failure findings")
}

func TestFailureAnalyzer_KindUnknownWithoutStepData(t *testing.T) {
	// Failures with no failing-step attribution (e.g. steps not fetched)
	// give no evidence for the flaky-vs-systematic call.
	details := makeConclusionDetails(map[string]int{
		conclusionSuccess: 5, conclusionFailure: 3,
	})
	// makeConclusionDetails attaches no failing steps.

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		AllDetails:    details,
		WorkflowNames: map[int64]string{100: "CI"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)
	assert.Equal(t, FailureKindUnknown, d.FailureKind,
		"no step data means no evidence for flaky vs systematic")
}

// makeConclusionDetails builds runs with the given conclusion counts (jobs
// carry the run's conclusion but no steps).
func makeConclusionDetails(counts map[string]int) []model.RunDetail {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail
	id := 1000
	for _, conclusion := range []string{conclusionSuccess, conclusionFailure, "skipped", "neutral", "cancelled"} {
		for range counts[conclusion] {
			start := base.Add(time.Duration(id-1000) * time.Hour)
			details = append(details, model.RunDetail{
				Run: model.WorkflowRun{
					ID: int64(id), WorkflowID: 100, WorkflowName: "CI",
					Status: "completed", Conclusion: conclusion, HeadSHA: "abc123",
					CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(5 * time.Minute),
				},
				Jobs: []model.Job{{Name: "build", Status: "completed", Conclusion: conclusion}},
			})
			id++
		}
	}
	return details
}

func TestFailureAnalyzer_DetectsFailures(t *testing.T) {
	details := makeFailureDetails()

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		AllDetails:    details,
		WorkflowNames: map[int64]string{100: "CI"},
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
	assert.Equal(t, 3, ciFailure.FailureCount) // only actual failures, not cancelled
	assert.InDelta(t, 0.15, ciFailure.FailureRate, 0.01)
	assert.Equal(t, 2, ciFailure.CancellationCount)
	assert.InDelta(t, 0.10, ciFailure.CancellationRate, 0.01)
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
				Status: "completed", Conclusion: conclusionSuccess,
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

func TestFailureAnalyzer_FailingStepAttribution(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail

	for i := range 10 {
		start := base.Add(time.Duration(i) * time.Hour)
		conclusion := conclusionSuccess
		jobConclusion := conclusionSuccess
		stepConclusion := conclusionSuccess
		if i%3 == 0 { // runs 0, 3, 6, 9 fail
			conclusion = conclusionFailure
			jobConclusion = conclusionFailure
			stepConclusion = conclusionFailure
		}
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(3000 + i), WorkflowID: 300, WorkflowName: "Tests",
				Status: "completed", Conclusion: conclusion,
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(5 * time.Minute),
			},
			Jobs: []model.Job{
				{
					Name: "integration", Status: "completed", Conclusion: jobConclusion,
					Steps: []model.Step{
						{Name: "Checkout", Status: "completed", Conclusion: conclusionSuccess},
						{Name: "Run tests", Status: "completed", Conclusion: stepConclusion},
					},
				},
			},
		})
	}

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{AllDetails: details})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)

	require.NotEmpty(t, d.FailingSteps, "should identify failing steps")
	assert.Equal(t, "Run tests", d.FailingSteps[0].StepName)
	assert.Equal(t, "integration", d.FailingSteps[0].JobName)
	assert.Equal(t, 4, d.FailingSteps[0].Count)
}

func TestFailureAnalyzer_CascadeFiltering(t *testing.T) {
	// When step "Run tests" fails, "Stop Docker Compose" also fails (cascade).
	// Only the root cause (first failing step) should be counted.
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail

	for i := range 10 {
		start := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(4000 + i), WorkflowID: 400, WorkflowName: "Tests",
				Status: "completed", Conclusion: conclusionFailure,
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(5 * time.Minute),
			},
			Jobs: []model.Job{{
				Name: "integration", Status: "completed", Conclusion: conclusionFailure,
				Steps: []model.Step{
					{Name: "Checkout", Number: 1, Status: "completed", Conclusion: conclusionSuccess},
					{Name: "Run tests", Number: 2, Status: "completed", Conclusion: conclusionFailure},
					{Name: "Stop Docker Compose", Number: 3, Status: "completed", Conclusion: conclusionFailure}, // cascade
				},
			}},
		})
	}

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{AllDetails: details})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)

	// Should only count "Run tests" (root cause), not "Stop Docker Compose" (cascade)
	require.Len(t, d.FailingSteps, 1, "cascade steps should be filtered")
	assert.Equal(t, "Run tests", d.FailingSteps[0].StepName)
	assert.Equal(t, 10, d.FailingSteps[0].Count)
}

func TestFailureAnalyzer_SystematicClassification(t *testing.T) {
	// All 20 failures hit the same step -> systematic
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail
	for i := range 30 {
		start := base.Add(time.Duration(i) * time.Hour)
		conclusion := conclusionSuccess
		if i < 20 {
			conclusion = conclusionFailure
		}
		run := model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(5000 + i), WorkflowID: 500, WorkflowName: "Review",
				Status: "completed", Conclusion: conclusion,
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(2 * time.Minute),
			},
		}
		if conclusion == conclusionFailure {
			run.Jobs = []model.Job{{
				Name: "review", Status: "completed", Conclusion: conclusionFailure,
				Steps: []model.Step{
					{Name: "Checkout", Number: 1, Conclusion: conclusionSuccess},
					{Name: "Run Review Bot", Number: 2, Conclusion: conclusionFailure},
				},
			}}
		}
		details = append(details, run)
	}

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{AllDetails: details})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)
	assert.Equal(t, FailureKindSystematic, d.FailureKind, "100% same step should be systematic")
}

func TestFailureAnalyzer_FlakyClassification(t *testing.T) {
	// Failures spread across multiple steps -> flaky
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail
	failSteps := []string{"Setup runner", "Compile TS", "Run tests", "Lint check"}
	for i := range 20 {
		start := base.Add(time.Duration(i) * time.Hour)
		conclusion := conclusionSuccess
		if i < 12 {
			conclusion = conclusionFailure
		}
		run := model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(6000 + i), WorkflowID: 600, WorkflowName: "Tests",
				Status: "completed", Conclusion: conclusion,
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(5 * time.Minute),
			},
		}
		if conclusion == conclusionFailure {
			failStep := failSteps[i%len(failSteps)]
			run.Jobs = []model.Job{{
				Name: "build", Status: "completed", Conclusion: conclusionFailure,
				Steps: []model.Step{
					{Name: "Checkout", Number: 1, Conclusion: conclusionSuccess},
					{Name: failStep, Number: 2, Conclusion: conclusionFailure},
				},
			}}
		}
		details = append(details, run)
	}

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{AllDetails: details})
	require.NoError(t, err)
	require.NotEmpty(t, findings)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)
	assert.Equal(t, FailureKindFlaky, d.FailureKind, "distributed failures should be flaky")
	assert.Greater(t, len(d.ByCategory), 1, "should have multiple categories")
}

func TestCategorizeStep(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"Setup runner", FailureCategoryInfra},
		{"Setup runner (mise, yarn)", FailureCategoryInfra},
		{"Checkout", FailureCategoryInfra},
		{"Install dependencies", FailureCategoryInfra},
		{"Compile TypeScript code", FailureCategoryBuild},
		{"Lint and Format Check", FailureCategoryBuild},
		{"Build binary", FailureCategoryBuild},
		{"Run tests", FailureCategoryTest},
		{"E2E tests \"hedera-lab\"", FailureCategoryTest},
		{"Integration tests", FailureCategoryTest},
		{"Run SOUP tests (jest runner)", FailureCategoryTest},
		{"Run Code Review with Claude", FailureCategoryOther},
		{"Deploy infrastructure", FailureCategoryOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, categorizeStep(tt.name))
		})
	}
}

func TestFailureDetail_Type(t *testing.T) {
	d := FailureDetail{}
	assert.Equal(t, "failure", d.DetailType())
}
