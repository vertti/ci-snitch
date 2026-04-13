package analyze

import (
	"context"
	"fmt"
	"slices"
)

// FailureDetail contains reliability information for a workflow.
type FailureDetail struct {
	Workflow     string         `json:"workflow"`
	TotalRuns    int            `json:"total_runs"`
	FailureCount int            `json:"failure_count"`
	FailureRate  float64        `json:"failure_rate"`
	ByConclusion map[string]int `json:"by_conclusion"`
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

	type wfStats struct {
		total        int
		failures     int
		byConclusion map[string]int
	}

	stats := make(map[string]*wfStats)
	for _, d := range ac.AllDetails {
		name := d.Run.WorkflowName
		if stats[name] == nil {
			stats[name] = &wfStats{byConclusion: make(map[string]int)}
		}
		s := stats[name]
		s.total++
		if d.Run.Conclusion != "success" && d.Run.Conclusion != "skipped" {
			s.failures++
			s.byConclusion[d.Run.Conclusion]++
		}
	}

	var findings []Finding
	for name, s := range stats {
		if s.failures == 0 {
			continue
		}
		rate := float64(s.failures) / float64(s.total)

		severity := SeverityInfo
		switch {
		case rate >= 0.2:
			severity = SeverityCritical
		case rate >= 0.05:
			severity = SeverityWarning
		}

		findings = append(findings, Finding{
			Type:     "failure",
			Severity: severity,
			Title:    fmt.Sprintf("Workflow %q failure rate", name),
			Description: fmt.Sprintf("%.0f%% failure rate (%d/%d runs)",
				rate*100, s.failures, s.total),
			Detail: FailureDetail{
				Workflow:     name,
				TotalRuns:    s.total,
				FailureCount: s.failures,
				FailureRate:  rate,
				ByConclusion: s.byConclusion,
			},
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
