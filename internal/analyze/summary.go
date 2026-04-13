package analyze

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// SummaryStats holds statistical measures for a duration series.
type SummaryStats struct {
	TotalRuns       int           `json:"total_runs"`
	Mean            time.Duration `json:"mean"`
	Median          time.Duration `json:"median"`
	P95             time.Duration `json:"p95"`
	P99             time.Duration `json:"p99"`
	Min             time.Duration `json:"min"`
	Max             time.Duration `json:"max"`
	TotalTime       time.Duration `json:"total_time"`
	Volatility      float64       `json:"volatility"`
	VolatilityLabel string        `json:"volatility_label"`
}

// SummaryDetail contains summary statistics for a workflow and its jobs.
type SummaryDetail struct {
	Workflow string       `json:"workflow"`
	Stats    SummaryStats `json:"stats"`
	Jobs     []JobSummary `json:"jobs"`
}

// JobSummary holds stats for a single job within a workflow.
type JobSummary struct {
	Name  string       `json:"name"`
	Stats SummaryStats `json:"stats"`
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
	type jobKey struct {
		wfID int64
		job  string
	}
	wfDurations := make(map[int64][]time.Duration)
	jobDurations := make(map[jobKey][]time.Duration)

	for _, d := range ac.Details {
		wfID := d.Run.WorkflowID
		dur := d.Run.Duration()
		if dur > 0 {
			wfDurations[wfID] = append(wfDurations[wfID], dur)
		}
		for _, j := range d.Jobs {
			dur := j.Duration()
			if dur > 0 {
				k := jobKey{wfID, j.Name}
				jobDurations[k] = append(jobDurations[k], dur)
			}
		}
	}

	// Build per-workflow summaries with nested jobs
	var findings []Finding
	for wfID, durations := range wfDurations {
		wfName := ac.WorkflowName(wfID)
		wfStats := computeStats(durations)

		// Collect jobs for this workflow
		var jobs []JobSummary
		for key, jDurations := range jobDurations {
			if key.wfID == wfID {
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

const (
	volatileThreshold = 3.0
	spikyThreshold    = 2.0
	variableThreshold = 1.3
)

func volatilityLabel(v float64) string {
	switch {
	case v >= volatileThreshold:
		return "volatile"
	case v >= spikyThreshold:
		return "spiky"
	case v >= variableThreshold:
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
