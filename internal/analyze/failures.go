package analyze

import (
	"context"
	"fmt"
	"slices"
)

const (
	criticalFailureRate = 0.20
	warningFailureRate  = 0.05
)

// FailureDetail contains reliability information for a workflow.
type FailureDetail struct {
	Workflow          string         `json:"workflow"`
	TotalRuns         int            `json:"total_runs"`
	FailureCount      int            `json:"failure_count"`
	FailureRate       float64        `json:"failure_rate"`
	CancellationCount int            `json:"cancellation_count"`
	CancellationRate  float64        `json:"cancellation_rate"`
	ByConclusion      map[string]int `json:"by_conclusion"`
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
			// Attribute failure to specific steps
			for _, j := range d.Jobs {
				if j.Conclusion != "failure" {
					continue
				}
				for _, st := range j.Steps {
					if st.Conclusion == "failure" {
						s.failingSteps[stepKey{j.Name, st.Name}]++
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
		for k, count := range s.failingSteps {
			failingSteps = append(failingSteps, FailingStep{
				JobName:  k.job,
				StepName: k.step,
				Count:    count,
			})
		}
		slices.SortFunc(failingSteps, func(a, b FailingStep) int {
			if b.Count != a.Count {
				return b.Count - a.Count
			}
			return 0
		})

		detail := FailureDetail{
			Workflow:          wfName,
			TotalRuns:         s.total,
			FailureCount:      s.failures,
			FailureRate:       failRate,
			CancellationCount: s.cancellations,
			CancellationRate:  cancelRate,
			ByConclusion:      s.byConclusion,
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
