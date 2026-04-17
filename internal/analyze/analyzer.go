// Package analyze provides the analysis engine and analyzer interface for CI performance analysis.
package analyze

import (
	"context"
	"fmt"

	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/preprocess"
)

// Analyzer examines workflow run data and produces findings.
type Analyzer interface {
	Name() string
	Analyze(ctx context.Context, ac *AnalysisContext) ([]Finding, error)
}

// AnalysisContext carries run data and lazily-computed derived views shared across analyzers.
type AnalysisContext struct {
	Details       []model.RunDetail               // filtered (success-only by default)
	AllDetails    []model.RunDetail               // unfiltered — includes failures, for reliability analysis
	RerunStats    map[int64]preprocess.RerunStats // per-workflow retry stats (computed before dedup)
	WorkflowNames map[int64]string                // WorkflowID → canonical name from ListWorkflows
}

// WorkflowName resolves the canonical workflow name for a given ID.
func (ac *AnalysisContext) WorkflowName(id int64) string {
	if name, ok := ac.WorkflowNames[id]; ok {
		return name
	}
	return fmt.Sprintf("workflow-%d", id)
}

// Severity levels for findings.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Change point directions.
const (
	DirectionSlowdown = "slowdown"
	DirectionSpeedup  = "speedup"
)

// Finding type identifiers.
const (
	TypeSummary     = "summary"
	TypeSteps       = "steps"
	TypeOutlier     = "outlier"
	TypeChangepoint = "changepoint"
	TypeFailure     = "failure"
	TypeCost        = "cost"
)

// Change point persistence classifications.
const (
	PersistencePersistent   = "persistent"
	PersistenceTransient    = "transient"
	PersistenceInconclusive = "inconclusive"
)

// Change point categories (set by post-processing).
const (
	CategoryRegression  = "regression"  // actionable slowdown (deduplicated, latest per job)
	CategoryOscillating = "oscillating" // volatile job with 3+ shifts (noise)
	CategoryMinor       = "minor"       // severity=info, hidden by default
	CategorySpeedup     = "speedup"     // improvement
)

// OutlierGroupDetail is a post-processed grouped view of outliers for a (workflow, job).
type OutlierGroupDetail struct {
	WorkflowName    string   `json:"workflow_name"`
	JobName         string   `json:"job_name,omitempty"`
	Count           int      `json:"count"`
	WorstDuration   Duration `json:"worst_duration"`
	WorstPercentile float64  `json:"worst_percentile"`
	WorstCommitSHA  string   `json:"worst_commit_sha"`
	MaxSeverity     string   `json:"max_severity"`
}

// DetailType implements FindingDetail.
func (OutlierGroupDetail) DetailType() string { return TypeOutlier }

// Finding represents a single analysis result.
type Finding struct {
	Type        string        `json:"type"`
	Severity    string        `json:"severity"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Detail      FindingDetail `json:"detail"`
}

// FindingDetail is implemented by typed detail structs for each analyzer.
type FindingDetail interface {
	DetailType() string
}
