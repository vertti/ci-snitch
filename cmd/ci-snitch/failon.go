package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// exitCodeError carries a specific process exit code through cobra's error
// path: 2 = a --fail-on gate tripped (data-driven), 1 = operational error.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }
func (e *exitCodeError) Code() int     { return e.code }

const (
	failOnRegression  = "regression"
	failOnFailureRate = "failure-rate"
	failOnPartialData = "partial-data"
)

// failOnCondition is one parsed --fail-on condition.
type failOnCondition struct {
	kind      string  // one of the failOn* constants
	threshold float64 // percent, for failure-rate
}

// parseFailOn parses a comma-separated --fail-on spec:
// "regression", "failure-rate>N", "partial-data", in any combination.
func parseFailOn(spec string) ([]failOnCondition, error) {
	if spec == "" {
		return nil, nil
	}
	var conds []failOnCondition
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		switch {
		case part == failOnRegression:
			conds = append(conds, failOnCondition{kind: failOnRegression})
		case part == failOnPartialData:
			conds = append(conds, failOnCondition{kind: failOnPartialData})
		case strings.HasPrefix(part, "failure-rate>"):
			raw := strings.TrimPrefix(part, "failure-rate>")
			threshold, err := strconv.ParseFloat(raw, 64)
			if err != nil || threshold < 0 || threshold >= 100 {
				return nil, fmt.Errorf("--fail-on failure-rate threshold must be a percentage in [0, 100), got %q", raw)
			}
			conds = append(conds, failOnCondition{kind: failOnFailureRate, threshold: threshold})
		default:
			return nil, fmt.Errorf("unknown --fail-on condition %q (supported: regression, failure-rate>N, partial-data)", part)
		}
	}
	return conds, nil
}

// evaluateFailOn returns one reason string per finding or incomplete workflow
// that trips a condition.
func evaluateFailOn(conds []failOnCondition, result *analyze.AnalysisResult) []string {
	var reasons []string
	for _, cond := range conds {
		if cond.kind == failOnPartialData {
			for _, w := range result.Meta.Incomplete {
				reasons = append(reasons, fmt.Sprintf("partial data: %s has details for %d of %d runs",
					w.Workflow, w.Hydrated, w.Listed))
			}
			continue
		}
		for _, f := range result.Findings {
			switch cond.kind {
			case failOnRegression:
				d, ok := f.Detail.(analyze.ChangePointDetail)
				if ok && d.Category == analyze.CategoryRegression {
					reasons = append(reasons, fmt.Sprintf("regression: %s / %s %+.0f%%",
						d.WorkflowName, d.JobName, d.PctChange))
				}
			case failOnFailureRate:
				d, ok := f.Detail.(analyze.FailureDetail)
				if ok && d.FailureRate*100 > cond.threshold {
					reasons = append(reasons, fmt.Sprintf("failure rate: %s at %.0f%% (threshold %.0f%%)",
						d.Workflow, d.FailureRate*100, cond.threshold))
				}
			}
		}
	}
	return reasons
}
