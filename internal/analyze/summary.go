package analyze

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// SummaryStats holds statistical measures for a duration series.
type SummaryStats struct {
	TotalRuns       int
	Mean            time.Duration
	Median          time.Duration
	P95             time.Duration
	P99             time.Duration
	Min             time.Duration
	Max             time.Duration
	TotalTime       time.Duration // sum of all durations (for ranking)
	Volatility      float64       // p95/median ratio — higher means more unpredictable
	VolatilityLabel string        // "stable", "variable", "spiky", or "volatile"
}

// SummaryDetail contains summary statistics for a workflow and its jobs.
type SummaryDetail struct {
	Workflow string
	Stats    SummaryStats
	Jobs     []JobSummary // sorted by median duration descending
}

// JobSummary holds stats for a single job within a workflow.
type JobSummary struct {
	Name  string
	Stats SummaryStats
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

	// Collect durations per workflow and per (workflow, job)
	type jobKey struct{ wf, job string }
	wfDurations := make(map[string][]time.Duration)
	jobDurations := make(map[jobKey][]time.Duration)

	for _, d := range ac.Details {
		wfName := d.Run.WorkflowName
		dur := d.Run.Duration()
		if dur > 0 {
			wfDurations[wfName] = append(wfDurations[wfName], dur)
		}
		for _, j := range d.Jobs {
			dur := j.Duration()
			if dur > 0 {
				jobDurations[jobKey{wfName, j.Name}] = append(jobDurations[jobKey{wfName, j.Name}], dur)
			}
		}
	}

	// Build per-workflow summaries with nested jobs
	var findings []Finding
	for wfName, durations := range wfDurations {
		wfStats := computeStats(durations)

		// Collect jobs for this workflow
		var jobs []JobSummary
		for key, jDurations := range jobDurations {
			if key.wf == wfName {
				jobs = append(jobs, JobSummary{
					Name:  key.job,
					Stats: computeStats(jDurations),
				})
			}
		}

		// Sort jobs by median descending (slowest first)
		slices.SortFunc(jobs, func(a, b JobSummary) int {
			return int(b.Stats.Median - a.Stats.Median)
		})

		detail := SummaryDetail{
			Workflow: wfName,
			Stats:    wfStats,
			Jobs:     jobs,
		}

		findings = append(findings, Finding{
			Type:     "summary",
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("Workflow %q", wfName),
			Description: fmt.Sprintf("%d runs, median %s, p95 %s, total CI time %s",
				wfStats.TotalRuns,
				wfStats.Median.Round(time.Second),
				wfStats.P95.Round(time.Second),
				wfStats.TotalTime.Round(time.Second)),
			Detail: detail,
		})
	}

	// Sort findings by total CI time descending (most expensive first)
	slices.SortFunc(findings, func(a, b Finding) int {
		ad, aOK := a.Detail.(SummaryDetail)
		bd, bOK := b.Detail.(SummaryDetail)
		if !aOK || !bOK {
			return 0
		}
		return int(bd.Stats.TotalTime - ad.Stats.TotalTime)
	})

	return findings, nil
}

func computeStats(durations []time.Duration) SummaryStats {
	slices.Sort(durations)

	n := len(durations)
	var total time.Duration
	for _, d := range durations {
		total += d
	}

	median := percentile(durations, 50)
	p95 := percentile(durations, 95)
	volatility := 0.0
	if median > 0 {
		volatility = float64(p95) / float64(median)
	}

	return SummaryStats{
		TotalRuns:       n,
		Mean:            total / time.Duration(n),
		Median:          median,
		P95:             p95,
		P99:             percentile(durations, 99),
		Min:             durations[0],
		Max:             durations[n-1],
		TotalTime:       total,
		Volatility:      volatility,
		VolatilityLabel: volatilityLabel(volatility),
	}
}

func volatilityLabel(v float64) string {
	switch {
	case v >= 3.0:
		return "volatile"
	case v >= 2.0:
		return "spiky"
	case v >= 1.3:
		return "variable"
	default:
		return "stable"
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
