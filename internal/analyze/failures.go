package analyze

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vertti/ci-snitch/internal/model"
)

// Failure trend directions.
const (
	FailureTrendImproving = "improving"
	FailureTrendWorsening = "worsening"
	FailureTrendStable    = "stable"
)

const (
	criticalFailureRate = 0.20
	warningFailureRate  = 0.05
)

// Failure kind classifications.
const (
	FailureKindSystematic = "systematic" // >90% of failures hit the same root-cause step
	FailureKindFlaky      = "flaky"      // failures spread across multiple steps
)

// Failure category classifications based on step name heuristics.
const (
	FailureCategoryInfra = "infra" // setup, runner, environment steps
	FailureCategoryBuild = "build" // compile, lint, build steps
	FailureCategoryTest  = "test"  // test, e2e, integration steps
	FailureCategoryOther = "other"
)

// FailureDetail contains reliability information for a workflow.
type FailureDetail struct {
	Workflow          string         `json:"workflow"`
	TotalRuns         int            `json:"total_runs"`
	FailureCount      int            `json:"failure_count"`
	FailureRate       float64        `json:"failure_rate"`
	FailureKind       string         `json:"failure_kind"`
	Trend             string         `json:"trend"`               // improving, worsening, stable
	RecentFailureRate float64        `json:"recent_failure_rate"` // failure rate in last 7 days
	CancellationCount int            `json:"cancellation_count"`
	CancellationRate  float64        `json:"cancellation_rate"`
	ByConclusion      map[string]int `json:"by_conclusion"`
	ByCategory        map[string]int `json:"by_category,omitempty"`
	FailingSteps      []FailingStep  `json:"failing_steps,omitempty"`
	RetriedRuns       int            `json:"retried_runs"`
	ExtraAttempts     int            `json:"extra_attempts"`
	RerunRate         float64        `json:"rerun_rate"`
}

// FailingStep identifies a step that frequently causes job failures.
type FailingStep struct {
	JobName  string `json:"job_name"`
	StepName string `json:"step_name"`
	Count    int    `json:"count"`
	Category string `json:"category"`
}

// DetailType implements FindingDetail.
func (FailureDetail) DetailType() string { return TypeFailure }

// FailureAnalyzer analyzes failure rates across workflows.
// Uses AllDetails (unfiltered) from AnalysisContext.
type FailureAnalyzer struct{}

// Name implements Analyzer.
func (FailureAnalyzer) Name() string { return TypeFailure }

type failureStepKey struct {
	job  string
	step string
}

type workflowFailureStat struct {
	total          int
	failures       int
	cancellations  int
	recentTotal    int
	recentFailures int
	byConclusion   map[string]int
	failingSteps   map[failureStepKey]int
}

// Analyze implements Analyzer.
func (FailureAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.AllDetails) == 0 {
		return nil, nil
	}

	wfStats := collectFailureStats(ac.AllDetails)

	const minRunsForFailureRate = 5
	var findings []Finding
	for wfID, s := range wfStats {
		if (s.failures == 0 && s.cancellations == 0) || s.total < minRunsForFailureRate {
			continue
		}
		finding := buildFailureFinding(ac, wfID, s)
		findings = append(findings, finding)
	}

	slices.SortFunc(findings, func(a, b Finding) int {
		ad, _ := a.Detail.(FailureDetail)
		bd, _ := b.Detail.(FailureDetail)
		if bd.FailureRate > ad.FailureRate {
			return 1
		}
		if bd.FailureRate < ad.FailureRate {
			return -1
		}
		return 0
	})

	return findings, nil
}

func collectFailureStats(details []model.RunDetail) map[int64]*workflowFailureStat {
	var latest time.Time
	for i := range details {
		if details[i].Run.CreatedAt.After(latest) {
			latest = details[i].Run.CreatedAt
		}
	}
	recentCutoff := latest.AddDate(0, 0, -7)

	wfStats := make(map[int64]*workflowFailureStat)
	for i := range details {
		wfID := details[i].Run.WorkflowID
		if wfStats[wfID] == nil {
			wfStats[wfID] = &workflowFailureStat{
				byConclusion: make(map[string]int),
				failingSteps: make(map[failureStepKey]int),
			}
		}
		s := wfStats[wfID]
		s.total++
		isRecent := details[i].Run.CreatedAt.After(recentCutoff)
		if isRecent {
			s.recentTotal++
		}
		switch details[i].Run.Conclusion {
		case "success", "skipped":
			// not a failure
		case "cancelled":
			s.cancellations++
			s.byConclusion[details[i].Run.Conclusion]++
		default:
			s.failures++
			if isRecent {
				s.recentFailures++
			}
			s.byConclusion[details[i].Run.Conclusion]++
			for j := range details[i].Jobs {
				if details[i].Jobs[j].Conclusion != "failure" {
					continue
				}
				for st := range details[i].Jobs[j].Steps {
					if details[i].Jobs[j].Steps[st].Conclusion == "failure" {
						s.failingSteps[failureStepKey{details[i].Jobs[j].Name, details[i].Jobs[j].Steps[st].Name}]++
						break
					}
				}
			}
		}
	}
	return wfStats
}

func buildFailureFinding(ac *AnalysisContext, wfID int64, s *workflowFailureStat) Finding {
	wfName := ac.WorkflowName(wfID)
	failRate := float64(s.failures) / float64(s.total)
	cancelRate := float64(s.cancellations) / float64(s.total)

	severity := SeverityInfo
	switch {
	case failRate >= criticalFailureRate:
		severity = SeverityCritical
	case failRate >= warningFailureRate:
		severity = SeverityWarning
	}

	var failingSteps []FailingStep
	byCategory := make(map[string]int)
	for k, count := range s.failingSteps {
		cat := categorizeStep(k.step)
		failingSteps = append(failingSteps, FailingStep{
			JobName:  k.job,
			StepName: k.step,
			Count:    count,
			Category: cat,
		})
		byCategory[cat] += count
	}
	slices.SortFunc(failingSteps, func(a, b FailingStep) int {
		return b.Count - a.Count
	})

	kind := FailureKindFlaky
	if len(failingSteps) > 0 && s.failures > 0 {
		if float64(failingSteps[0].Count)/float64(s.failures) >= 0.9 {
			kind = FailureKindSystematic
		}
	}

	var recentRate float64
	trend := FailureTrendStable
	if s.recentTotal >= 5 {
		recentRate = float64(s.recentFailures) / float64(s.recentTotal)
		diff := recentRate - failRate
		if diff <= -0.05 {
			trend = FailureTrendImproving
		} else if diff >= 0.05 {
			trend = FailureTrendWorsening
		}
	}

	detail := FailureDetail{
		Workflow:          wfName,
		TotalRuns:         s.total,
		FailureCount:      s.failures,
		FailureRate:       failRate,
		FailureKind:       kind,
		Trend:             trend,
		RecentFailureRate: recentRate,
		CancellationCount: s.cancellations,
		CancellationRate:  cancelRate,
		ByConclusion:      s.byConclusion,
		ByCategory:        byCategory,
		FailingSteps:      failingSteps,
	}
	if rs, ok := ac.RerunStats[wfID]; ok {
		detail.RetriedRuns = rs.RetriedRuns
		detail.ExtraAttempts = rs.ExtraAttempts
		detail.RerunRate = rs.RerunRate
	}

	return Finding{
		Type:     TypeFailure,
		Severity: severity,
		Title:    fmt.Sprintf("Workflow %q failure rate", wfName),
		Description: fmt.Sprintf("%.0f%% failure rate (%d/%d runs)",
			failRate*100, s.failures, s.total),
		Detail: detail,
	}
}

// categorizeStep classifies a step name into infra/build/test/other.
func categorizeStep(name string) string {
	lower := strings.ToLower(name)
	// Check test first — more specific, avoids false infra matches on "runner" in "jest runner"
	switch {
	case strings.Contains(lower, "test") ||
		strings.Contains(lower, "e2e") ||
		strings.Contains(lower, "integration") ||
		strings.Contains(lower, "spec") ||
		strings.Contains(lower, "jest") ||
		strings.Contains(lower, "pytest"):
		return FailureCategoryTest
	case strings.Contains(lower, "compile") ||
		strings.Contains(lower, "lint") ||
		strings.Contains(lower, "build") ||
		strings.Contains(lower, "format"):
		return FailureCategoryBuild
	case strings.Contains(lower, "setup") ||
		strings.Contains(lower, "runner") ||
		strings.Contains(lower, "checkout") ||
		strings.Contains(lower, "cache") ||
		strings.Contains(lower, "install") ||
		strings.Contains(lower, "docker compose"):
		return FailureCategoryInfra
	default:
		return FailureCategoryOther
	}
}
