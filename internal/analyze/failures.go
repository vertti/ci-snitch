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
	Workflow      string         `json:"workflow"`
	TotalRuns     int            `json:"total_runs"`
	FailureCount  int            `json:"failure_count"`
	FailureRate   float64        `json:"failure_rate"`
	ByConclusion  map[string]int `json:"by_conclusion"`
	RetriedRuns   int            `json:"retried_runs"`
	ExtraAttempts int            `json:"extra_attempts"`
	RerunRate     float64        `json:"rerun_rate"`
}

// DetailType implements FindingDetail.
func (FailureDetail) DetailType() string { return "failure" }

// FailureAnalyzer analyzes failure rates across workflows.
// Uses AllDetails (unfiltered) from AnalysisContext.
type FailureAnalyzer struct{}

// Name implements Analyzer.
func (FailureAnalyzer) Name() string { return "failure" }

// Analyze implements Analyzer.
func (FailureAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.AllDetails) == 0 {
		return nil, nil
	}

	type wfStat struct {
		total        int
		failures     int
		byConclusion map[string]int
	}

	wfStats := make(map[int64]*wfStat)
	for _, d := range ac.AllDetails {
		wfID := d.Run.WorkflowID
		if wfStats[wfID] == nil {
			wfStats[wfID] = &wfStat{byConclusion: make(map[string]int)}
		}
		s := wfStats[wfID]
		s.total++
		if d.Run.Conclusion != "success" && d.Run.Conclusion != "skipped" {
			s.failures++
			s.byConclusion[d.Run.Conclusion]++
		}
	}

	var findings []Finding
	for wfID, s := range wfStats {
		if s.failures == 0 {
			continue
		}
		wfName := ac.WorkflowName(wfID)
		rate := float64(s.failures) / float64(s.total)

		severity := SeverityInfo
		switch {
		case rate >= criticalFailureRate:
			severity = SeverityCritical
		case rate >= warningFailureRate:
			severity = SeverityWarning
		}

		detail := FailureDetail{
			Workflow:     wfName,
			TotalRuns:    s.total,
			FailureCount: s.failures,
			FailureRate:  rate,
			ByConclusion: s.byConclusion,
		}
		if rs, ok := ac.RerunStats[wfID]; ok {
			detail.RetriedRuns = rs.RetriedRuns
			detail.ExtraAttempts = rs.ExtraAttempts
			detail.RerunRate = rs.RerunRate
		}

		findings = append(findings, Finding{
			Type:     "failure",
			Severity: severity,
			Title:    fmt.Sprintf("Workflow %q failure rate", wfName),
			Description: fmt.Sprintf("%.0f%% failure rate (%d/%d runs)",
				rate*100, s.failures, s.total),
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
