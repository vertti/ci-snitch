package main

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/analyze"
	"github.com/vertti/ci-snitch/internal/app"
	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/output"
)

func TestParseFailOn(t *testing.T) {
	tests := []struct {
		input   string
		wantErr string
	}{
		{input: "regression"},
		{input: "failure-rate>25"},
		{input: "regression,failure-rate>10"},
		{input: "partial-data"},
		{input: "regression,partial-data"},
		{input: "bogus", wantErr: "unknown --fail-on condition"},
		{input: "failure-rate>abc", wantErr: "threshold"},
		{input: "failure-rate>-5", wantErr: "threshold"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			_, err := parseFailOn(tt.input)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestFailOnEvaluate(t *testing.T) {
	result := &analyze.AnalysisResult{Findings: []analyze.Finding{
		{Type: analyze.TypeChangepoint, Detail: analyze.ChangePointDetail{
			WorkflowName: "CI", JobName: "build", Category: analyze.CategoryRegression, PctChange: 40,
		}},
		{Type: analyze.TypeChangepoint, Detail: analyze.ChangePointDetail{
			WorkflowName: "CI", JobName: "lint", Category: analyze.CategorySpeedup, PctChange: -20,
		}},
	}, Reliability: []analyze.WorkflowReliability{
		{Workflow: "CI", TotalRuns: 100, FailureCount: 15, FailureRate: 0.15},
	}}

	conds, err := parseFailOn("regression")
	require.NoError(t, err)
	reasons := evaluateFailOn(conds, result)
	require.Len(t, reasons, 1, "one regression must trip the gate")
	assert.Contains(t, reasons[0], "build")

	conds, err = parseFailOn("failure-rate>10")
	require.NoError(t, err)
	reasons = evaluateFailOn(conds, result)
	require.Len(t, reasons, 1, "15% > 10% must trip")
	assert.Contains(t, reasons[0], "15%")

	conds, err = parseFailOn("failure-rate>20")
	require.NoError(t, err)
	assert.Empty(t, evaluateFailOn(conds, result), "15% is under a 20% threshold")

	// A speedup is not a regression.
	noRegression := &analyze.AnalysisResult{Findings: result.Findings[1:]}
	conds, _ = parseFailOn("regression")
	assert.Empty(t, evaluateFailOn(conds, noRegression))
}

func TestFailOnEvaluate_PartialDataTripsOncePerIncompleteWorkflow(t *testing.T) {
	conds, err := parseFailOn("partial-data")
	require.NoError(t, err)

	partial := &analyze.AnalysisResult{Meta: analyze.ResultMeta{Incomplete: []analyze.IncompleteWorkflow{
		{Workflow: "tests", Hydrated: 146, Listed: 854},
		{Workflow: "deploy", Hydrated: 61, Listed: 235},
	}}}
	reasons := evaluateFailOn(conds, partial)
	require.Len(t, reasons, 2)
	assert.Equal(t, "partial data: tests has details for 146 of 854 runs", reasons[0])

	assert.Empty(t, evaluateFailOn(conds, &analyze.AnalysisResult{}))
}

func TestExitCodeError(t *testing.T) {
	err := &exitCodeError{code: 2, msg: "gate tripped"}
	assert.Equal(t, 2, err.Code())
	assert.Equal(t, "gate tripped", err.Error())
}

func TestApplyFailOnGate_PrintsReasonsAndExits2(t *testing.T) {
	result := &analyze.AnalysisResult{Findings: []analyze.Finding{{
		Type: analyze.TypeChangepoint,
		Detail: analyze.ChangePointDetail{
			JobName:   "build",
			Category:  analyze.CategoryRegression,
			Direction: analyze.DirectionSlowdown,
			PctChange: 25,
		},
	}}}

	var buf bytes.Buffer
	err := applyFailOnGate(&buf, []failOnCondition{{kind: failOnRegression}}, result)
	require.Error(t, err)

	var ec *exitCodeError
	require.ErrorAs(t, err, &ec)
	assert.Equal(t, 2, ec.code)
	assert.Contains(t, buf.String(), "fail-on:", "the reason must reach the writer so CI logs explain the failure")
	assert.Contains(t, buf.String(), "build")
}

func TestApplyFailOnGate_CleanResultPasses(t *testing.T) {
	var buf bytes.Buffer
	err := applyFailOnGate(&buf, []failOnCondition{{kind: failOnRegression}}, &analyze.AnalysisResult{})
	require.NoError(t, err)
	assert.Empty(t, buf.String())
}

// scanWithFailures runs the service over one workflow whose first `failures`
// runs failed.
func scanWithFailures(t *testing.T, runs, failures int) analyze.AnalysisResult {
	t.Helper()
	now := time.Now()
	var listed []model.WorkflowRun
	var details []model.RunDetail
	for i := range runs {
		conclusion := "success"
		if i < failures {
			conclusion = "failure"
		}
		started := now.Add(-time.Duration(runs-i) * time.Hour)
		run := model.WorkflowRun{
			ID: int64(100 + i), WorkflowID: 1, WorkflowName: "CI",
			Status: "completed", Conclusion: conclusion,
			HeadBranch: "main", HeadSHA: "abc123", RunAttempt: 1,
			CreatedAt: started, StartedAt: started, UpdatedAt: started.Add(10 * time.Minute),
		}
		listed = append(listed, run)
		details = append(details, model.RunDetail{Run: run, Jobs: []model.Job{{
			ID: int64(1000 + i), RunID: run.ID, Name: "build", Status: "completed", Conclusion: conclusion,
			StartedAt: started, CompletedAt: started.Add(10 * time.Minute),
			Labels: []string{"ubuntu-latest"},
		}}})
	}
	svc := &app.Service{
		Client: &stubFetcher{workflows: []model.Workflow{{ID: 1, Name: "CI"}}, runs: listed, details: details},
		Prog:   output.NewProgressQuiet(),
	}
	result, err := svc.Run(context.Background(), &app.Options{Repo: "test/repo", Since: now.Add(-30 * 24 * time.Hour)})
	require.NoError(t, err)
	return result
}

func TestFailOnFailureRate_CountsFailuresTheReportDoesNotShow(t *testing.T) {
	conds, err := parseFailOn("failure-rate>0")
	require.NoError(t, err)

	scans := map[string]struct{ runs, failures int }{
		"a rate under the 5% display threshold": {runs: 100, failures: 1},
		"a workflow with fewer than five runs":  {runs: 4, failures: 1},
	}
	for name, scan := range scans {
		t.Run(name, func(t *testing.T) {
			result := scanWithFailures(t, scan.runs, scan.failures)
			for _, f := range result.Findings {
				require.NotEqual(t, analyze.TypeFailure, f.Type, "the report still hides it")
			}

			reasons := evaluateFailOn(conds, &result)
			require.Len(t, reasons, 1)
			assert.Contains(t, reasons[0], fmt.Sprintf("%d of %d runs", scan.failures, scan.runs))
		})
	}
}
