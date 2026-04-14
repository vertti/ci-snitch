package analyze

import (
	"context"
	"fmt"
	"time"

	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/preprocess"
)

// Warning represents a non-fatal issue during analysis.
type Warning struct {
	Message string `json:"message"`
}

// ResultMeta contains metadata about the analysis run.
type ResultMeta struct {
	Repo        string       `json:"repo"`
	TotalRuns   int          `json:"total_runs"`
	TimeRange   [2]time.Time `json:"time_range"`
	WorkflowIDs []int64      `json:"workflow_ids"`
}

// AnalysisResult is the output of the analysis engine.
type AnalysisResult struct {
	Findings []Finding  `json:"findings"`
	Warnings []Warning  `json:"warnings"`
	Meta     ResultMeta `json:"meta"`
}

// Engine orchestrates running analyzers over a set of run details.
type Engine struct {
	analyzers []Analyzer
}

// NewEngine creates an engine with the given analyzers.
func NewEngine(analyzers ...Analyzer) *Engine {
	return &Engine{analyzers: analyzers}
}

// Run executes all analyzers sequentially and collects results.
// allDetails is optional unfiltered data for analyzers that need it (e.g. failure analysis).
// rerunStats is optional per-workflow retry stats (computed before dedup).
// workflowNames maps WorkflowID → canonical name from ListWorkflows.
func (e *Engine) Run(ctx context.Context, details, allDetails []model.RunDetail, rerunStats map[int64]preprocess.RerunStats, workflowNames map[int64]string) AnalysisResult {
	ac := &AnalysisContext{Details: details, AllDetails: allDetails, RerunStats: rerunStats, WorkflowNames: workflowNames}

	var result AnalysisResult
	result.Meta = computeMeta(details)

	for _, a := range e.analyzers {
		findings, err := a.Analyze(ctx, ac)
		if err != nil {
			result.Warnings = append(result.Warnings, Warning{
				Message: fmt.Sprintf("analyzer %q failed: %v", a.Name(), err),
			})
			continue
		}
		result.Findings = append(result.Findings, findings...)
	}

	result.Findings = postProcess(result.Findings)

	return result
}

func computeMeta(details []model.RunDetail) ResultMeta {
	meta := ResultMeta{TotalRuns: len(details)}
	wfSet := make(map[int64]bool)

	for _, d := range details {
		wfSet[d.Run.WorkflowID] = true
		t := d.Run.CreatedAt
		if meta.TimeRange[0].IsZero() || t.Before(meta.TimeRange[0]) {
			meta.TimeRange[0] = t
		}
		if t.After(meta.TimeRange[1]) {
			meta.TimeRange[1] = t
		}
	}

	for id := range wfSet {
		meta.WorkflowIDs = append(meta.WorkflowIDs, id)
	}
	return meta
}
