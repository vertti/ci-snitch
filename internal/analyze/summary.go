package analyze

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/vertti/ci-snitch/internal/preprocess"
)

// SummaryStats holds statistical measures for a duration series.
type SummaryStats struct {
	TotalRuns       int      `json:"total_runs"`
	Mean            Duration `json:"mean"`
	Median          Duration `json:"median"`
	P95             Duration `json:"p95"`
	P99             Duration `json:"p99"`
	Min             Duration `json:"min"`
	Max             Duration `json:"max"`
	TotalTime       Duration `json:"total_time"`
	Volatility      float64  `json:"volatility"`
	VolatilityLabel string   `json:"volatility_label"`
}

// QueueStats holds queue/wait time statistics (CreatedAt to StartedAt gap).
type QueueStats struct {
	Median Duration `json:"median"`
	P95    Duration `json:"p95"`
}

// SummaryDetail contains summary statistics for a workflow and its jobs.
type SummaryDetail struct {
	Workflow string       `json:"workflow"`
	Stats    SummaryStats `json:"stats"`
	Queue    QueueStats   `json:"queue"`
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
type SummaryAnalyzer struct {
	// GroupMatrix groups matrix job variants (e.g. "test (ubuntu, 20)" and "test (macos, 20)")
	// under a single "test" entry with aggregate stats. Default: true.
	GroupMatrix *bool
}

// Name implements Analyzer.
func (SummaryAnalyzer) Name() string { return "summary" }

// Analyze implements Analyzer.
func (s SummaryAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) == 0 {
		return nil, nil
	}

	// Collect durations per workflow and per (workflow, job)
	wfDurations := make(map[int64][]time.Duration)
	wfQueueTimes := make(map[int64][]time.Duration)
	jobDurations := make(map[jobKey][]time.Duration)

	for _, d := range ac.Details {
		wfID := d.Run.WorkflowID
		dur := d.Duration()
		if dur > 0 {
			wfDurations[wfID] = append(wfDurations[wfID], dur)
		}
		// Queue time: how long the run waited before starting
		if !d.Run.CreatedAt.IsZero() && !d.Run.StartedAt.IsZero() {
			qt := d.Run.StartedAt.Sub(d.Run.CreatedAt)
			if qt >= 0 {
				wfQueueTimes[wfID] = append(wfQueueTimes[wfID], qt)
			}
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

		// Group matrix job variants under a single entry
		if s.GroupMatrix == nil || *s.GroupMatrix {
			jobs = groupMatrixJobs(jobs, jobDurations, wfID)
		}

		// Sort jobs by median descending (slowest first)
		slices.SortFunc(jobs, func(a, b JobSummary) int {
			return int(b.Stats.Median - a.Stats.Median)
		})

		var queue QueueStats
		if qts := wfQueueTimes[wfID]; len(qts) > 0 {
			slices.Sort(qts)
			queue = QueueStats{
				Median: Duration(percentile(qts, 50)),
				P95:    Duration(percentile(qts, 95)),
			}
		}

		detail := SummaryDetail{
			Workflow: wfName,
			Stats:    wfStats,
			Queue:    queue,
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

type jobKey struct {
	wfID int64
	job  string
}

// groupMatrixJobs merges matrix variants (e.g. "test (ubuntu, 20)", "test (macos, 20)")
// into a single "test" entry with combined durations when there are multiple variants.
func groupMatrixJobs(jobs []JobSummary, jobDurations map[jobKey][]time.Duration, wfID int64) []JobSummary {
	// Group by base name
	type group struct {
		variants  []string
		durations []time.Duration
	}
	groups := make(map[string]*group)
	order := make([]string, 0) // preserve first-seen order

	for _, j := range jobs {
		base, _ := preprocess.ParseMatrixJobName(j.Name)
		g, ok := groups[base]
		if !ok {
			g = &group{}
			groups[base] = g
			order = append(order, base)
		}
		g.variants = append(g.variants, j.Name)
		g.durations = append(g.durations, jobDurations[jobKey{wfID, j.Name}]...)
	}

	// Only group if a base name has multiple variants
	var result []JobSummary
	for _, base := range order {
		g := groups[base]
		if len(g.variants) <= 1 {
			// Single variant — keep original name
			result = append(result, JobSummary{
				Name:  g.variants[0],
				Stats: computeStats(g.durations),
			})
		} else {
			result = append(result, JobSummary{
				Name:  fmt.Sprintf("%s (%d variants)", base, len(g.variants)),
				Stats: computeStats(g.durations),
			})
		}
	}
	return result
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
		Mean:            Duration(total / time.Duration(n)),
		Median:          Duration(median),
		P95:             Duration(p95),
		P99:             Duration(percentile(durations, 99)),
		Min:             Duration(durations[0]),
		Max:             Duration(durations[n-1]),
		TotalTime:       Duration(total),
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
