package analyze

import (
	"context"
	"fmt"
	"slices"
	"strings"
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

// Analyze implements Analyzer.
func (FailureAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.AllDetails) == 0 {
		return nil, nil
	}

	type stepKey struct {
		job  string
		step string
	}
	type wfStat struct {
		total         int
		failures      int
		cancellations int
		byConclusion  map[string]int
		failingSteps  map[stepKey]int
	}

	wfStats := make(map[int64]*wfStat)
	for _, d := range ac.AllDetails {
		wfID := d.Run.WorkflowID
		if wfStats[wfID] == nil {
			wfStats[wfID] = &wfStat{
				byConclusion: make(map[string]int),
				failingSteps: make(map[stepKey]int),
			}
		}
		s := wfStats[wfID]
		s.total++
		switch d.Run.Conclusion {
		case "success", "skipped":
			// not a failure
		case "cancelled":
			s.cancellations++
			s.byConclusion[d.Run.Conclusion]++
		default:
			s.failures++
			s.byConclusion[d.Run.Conclusion]++
			// Attribute failure to root-cause step (first failing step per job).
			// Later failing steps are often cascades (e.g. "Stop Docker Compose"
			// fails because a prior step already broke the environment).
			for _, j := range d.Jobs {
				if j.Conclusion != "failure" {
					continue
				}
				for _, st := range j.Steps {
					if st.Conclusion == "failure" {
						s.failingSteps[stepKey{j.Name, st.Name}]++
						break // only count the first (root-cause) failing step per job
					}
				}
			}
		}
	}

	const minRunsForFailureRate = 5

	var findings []Finding
	for wfID, s := range wfStats {
		if (s.failures == 0 && s.cancellations == 0) || s.total < minRunsForFailureRate {
			continue
		}
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
			if b.Count != a.Count {
				return b.Count - a.Count
			}
			return 0
		})

		// Classify as systematic (single root cause) vs flaky (distributed)
		kind := FailureKindFlaky
		if len(failingSteps) > 0 && s.failures > 0 {
			topRatio := float64(failingSteps[0].Count) / float64(s.failures)
			if topRatio >= 0.9 {
				kind = FailureKindSystematic
			}
		}

		detail := FailureDetail{
			Workflow:          wfName,
			TotalRuns:         s.total,
			FailureCount:      s.failures,
			FailureRate:       failRate,
			FailureKind:       kind,
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

		findings = append(findings, Finding{
			Type:     TypeFailure,
			Severity: severity,
			Title:    fmt.Sprintf("Workflow %q failure rate", wfName),
			Description: fmt.Sprintf("%.0f%% failure rate (%d/%d runs)",
				failRate*100, s.failures, s.total),
			Detail: detail,
		})
	}

	// Sort by failure rate descending
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
