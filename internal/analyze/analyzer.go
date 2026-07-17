// Package analyze provides the analysis engine and analyzer interface for CI performance analysis.
package analyze

import (
	"context"
	"fmt"
	"slices"
	"sync"

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

	jobSeriesOnce sync.Once
	jobSeries     map[JobKey]*JobSeries
}

// WorkflowName resolves the canonical workflow name for a given ID.
func (ac *AnalysisContext) WorkflowName(id int64) string {
	if name, ok := ac.WorkflowNames[id]; ok {
		return name
	}
	return fmt.Sprintf("workflow-%d", id)
}

// JobKey identifies a job's data within a workflow. Keying by job name alone
// would mix distributions from different workflows that happen to share a
// job name (every repo has a "build").
type JobKey struct {
	WorkflowID int64
	Job        string
}

// wfJobName keys post-processing state by display names — the identity that
// findings (and formatters) carry. Distinct from JobKey, which keys raw run
// data by workflow ID.
type wfJobName struct{ wf, job string }

// RunJobRef points one series sample back at its source run and job.
type RunJobRef struct {
	DetailIdx int // index into AnalysisContext.Details
	JobIdx    int // index into that detail's Jobs
}

// JobSeries holds one (workflow, job)'s positive job durations in run
// chronological order, with a back-reference per sample.
type JobSeries struct {
	Durations []float64 // seconds
	Refs      []RunJobRef
}

// JobSeries returns the per-(workflow, job) duration series over Details,
// ordered by run CreatedAt, computed once and shared — the changepoint and
// outlier analyzers previously each re-collected it. Zero and negative
// durations are excluded; Refs align 1:1 with Durations.
func (ac *AnalysisContext) JobSeries() map[JobKey]*JobSeries {
	ac.jobSeriesOnce.Do(func() {
		order := make([]int, len(ac.Details))
		for i := range order {
			order[i] = i
		}
		slices.SortStableFunc(order, func(a, b int) int {
			return ac.Details[a].Run.CreatedAt.Compare(ac.Details[b].Run.CreatedAt)
		})

		series := make(map[JobKey]*JobSeries)
		for _, di := range order {
			d := &ac.Details[di]
			for j := range d.Jobs {
				dur := d.Jobs[j].Duration().Seconds()
				if dur <= 0 {
					continue
				}
				k := JobKey{WorkflowID: d.Run.WorkflowID, Job: d.Jobs[j].Name}
				s := series[k]
				if s == nil {
					s = &JobSeries{}
					series[k] = s
				}
				s.Durations = append(s.Durations, dur)
				s.Refs = append(s.Refs, RunJobRef{DetailIdx: di, JobIdx: j})
			}
		}
		ac.jobSeries = series
	})
	return ac.jobSeries
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

// AllTypes lists every finding type. New types must be added here — the
// output package's exhaustiveness test fails any type this list contains
// that the formatters would silently drop.
var AllTypes = []string{
	TypeSummary, TypeSteps, TypePipeline, TypeRunner,
	TypeOutlier, TypeChangepoint, TypeFailure, TypeCost,
}

// Change point persistence classifications.
const (
	PersistencePersistent   = "persistent"
	PersistenceTransient    = "transient"
	PersistenceInconclusive = "inconclusive"
)

// Volatility labels (p95/median ratio buckets, see volatilityLabel).
// Exported because formatters key coloring and filtering on them.
const (
	VolatilityStable   = "stable"
	VolatilityVariable = "variable"
	VolatilitySpiky    = "spiky"
	VolatilityVolatile = "volatile"
)

// Runner sizing issues (RunnerDetail.Issue).
const (
	IssueOversized  = "oversized"
	IssueUndersized = "undersized"
)

// Commit kinds for regression attribution (ChangePointDetail.CommitKind).
const (
	CommitKindCIConfig = "ci-config"
	CommitKindCode     = "code"
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
