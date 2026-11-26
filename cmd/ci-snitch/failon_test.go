package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/analyze"
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
		{Type: analyze.TypeFailure, Detail: analyze.FailureDetail{
			Workflow: "CI", FailureRate: 0.15,
		}},
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
