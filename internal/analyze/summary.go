package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// SummaryDetail contains summary statistics for a workflow or job.
type SummaryDetail struct {
	Subject   string // workflow name or job name
	TotalRuns int
	Mean      time.Duration
	Median    time.Duration
	P95       time.Duration
	P99       time.Duration
	Min       time.Duration
	Max       time.Duration
}

// DetailType implements FindingDetail.
func (SummaryDetail) DetailType() string { return "summary" }

// SummaryAnalyzer computes per-workflow and per-job summary statistics.
type SummaryAnalyzer struct{}

// Name implements Analyzer.
func (SummaryAnalyzer) Name() string { return "summary" }

// Analyze implements Analyzer.
func (s SummaryAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) == 0 {
		return nil, nil
	}

	var findings []Finding

	// Per-workflow summary (using total run duration)
	wfDurations := make(map[string][]time.Duration)
	jobDurations := make(map[string][]time.Duration)

	for _, d := range ac.Details {
		dur := d.Run.Duration()
		if dur > 0 {
			wfDurations[d.Run.WorkflowName] = append(wfDurations[d.Run.WorkflowName], dur)
		}
		for _, j := range d.Jobs {
			dur := j.Duration()
			if dur > 0 {
				jobDurations[j.Name] = append(jobDurations[j.Name], dur)
			}
		}
	}

	for name, durations := range wfDurations {
		detail := computeSummary(name, durations)
		findings = append(findings, Finding{
			Type:     "summary",
			Severity: "info",
			Title:    fmt.Sprintf("Workflow %q summary", name),
			Description: fmt.Sprintf("%d runs, median %s, p95 %s",
				detail.TotalRuns, detail.Median.Round(time.Second), detail.P95.Round(time.Second)),
			Detail: detail,
		})
	}

	for name, durations := range jobDurations {
		detail := computeSummary(name, durations)
		findings = append(findings, Finding{
			Type:     "summary",
			Severity: "info",
			Title:    fmt.Sprintf("Job %q summary", name),
			Description: fmt.Sprintf("%d runs, median %s, p95 %s",
				detail.TotalRuns, detail.Median.Round(time.Second), detail.P95.Round(time.Second)),
			Detail: detail,
		})
	}

	return findings, nil
}

func computeSummary(subject string, durations []time.Duration) SummaryDetail {
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	n := len(durations)
	var total time.Duration
	for _, d := range durations {
		total += d
	}

	return SummaryDetail{
		Subject:   subject,
		TotalRuns: n,
		Mean:      total / time.Duration(n),
		Median:    percentile(durations, 50),
		P95:       percentile(durations, 95),
		P99:       percentile(durations, 99),
		Min:       durations[0],
		Max:       durations[n-1],
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := p / 100 * float64(len(sorted)-1)
	lower := int(idx)
	if lower >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lower)
	return sorted[lower] + time.Duration(frac*float64(sorted[lower+1]-sorted[lower]))
}
