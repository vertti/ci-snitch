package analyze

import (
	"context"
	"fmt"
	"time"

	"github.com/vertti/ci-snitch/internal/stats"
)

// OutlierDetail contains information about an outlier run or job.
type OutlierDetail struct {
	RunID        int64
	CommitSHA    string
	Duration     time.Duration
	Percentile   float64 // e.g. 97 means slower than 97% of runs
	WorkflowName string
	JobName      string // empty for workflow-level outliers
}

// DetailType implements FindingDetail.
func (OutlierDetail) DetailType() string { return "outlier" }

// OutlierAnalyzer detects runs or jobs with abnormally long durations.
type OutlierAnalyzer struct {
	// Method selects the outlier detection method: "log-iqr" (default) or "mad"
	Method string
	// MinPercentile is the minimum percentile to report (default: 95).
	// Outliers below this threshold are detected but not emitted as findings.
	MinPercentile float64
}

// Name implements Analyzer.
func (OutlierAnalyzer) Name() string { return "outlier" }

// Analyze implements Analyzer.
func (o OutlierAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) < 5 {
		return nil, nil
	}

	minPct := o.MinPercentile
	if minPct == 0 {
		minPct = 95
	}

	var findings []Finding

	// Workflow-level outliers
	wfDurations := make(map[string][]float64)
	wfRuns := make(map[string][]int) // index into ac.Details
	for i, d := range ac.Details {
		dur := d.Run.Duration().Seconds()
		if dur > 0 {
			wfDurations[d.Run.WorkflowName] = append(wfDurations[d.Run.WorkflowName], dur)
			wfRuns[d.Run.WorkflowName] = append(wfRuns[d.Run.WorkflowName], i)
		}
	}

	for wfName, durations := range wfDurations {
		idxMap := wfRuns[wfName]
		outliers := o.detect(durations)
		for _, out := range outliers {
			if out.Percentile < minPct {
				continue
			}
			detailIdx := idxMap[out.Index]
			d := ac.Details[detailIdx]
			findings = append(findings, Finding{
				Type:     "outlier",
				Severity: severityFromPercentile(out.Percentile),
				Title:    fmt.Sprintf("Slow run in %q", wfName),
				Description: fmt.Sprintf("Run took %s (p%.0f — slower than %.0f%% of runs)",
					d.Run.Duration().Round(time.Second), out.Percentile, out.Percentile),
				Detail: OutlierDetail{
					RunID:        d.Run.ID,
					CommitSHA:    d.Run.HeadSHA,
					Duration:     d.Run.Duration(),
					Percentile:   out.Percentile,
					WorkflowName: wfName,
				},
			})
		}
	}

	// Job-level outliers
	type jobKey struct {
		wf, job string
	}
	jobDurations := make(map[jobKey][]float64)
	type jobRef struct {
		detailIdx int
		jobIdx    int
	}
	jobRefs := make(map[jobKey][]jobRef)

	for i, d := range ac.Details {
		for j, job := range d.Jobs {
			dur := job.Duration().Seconds()
			if dur > 0 {
				k := jobKey{d.Run.WorkflowName, job.Name}
				jobDurations[k] = append(jobDurations[k], dur)
				jobRefs[k] = append(jobRefs[k], jobRef{i, j})
			}
		}
	}

	for k, durations := range jobDurations {
		refs := jobRefs[k]
		outliers := o.detect(durations)
		for _, out := range outliers {
			if out.Percentile < minPct {
				continue
			}
			ref := refs[out.Index]
			d := ac.Details[ref.detailIdx]
			job := d.Jobs[ref.jobIdx]
			findings = append(findings, Finding{
				Type:     "outlier",
				Severity: severityFromPercentile(out.Percentile),
				Title:    fmt.Sprintf("Slow job %q in %q", k.job, k.wf),
				Description: fmt.Sprintf("Job took %s (p%.0f — slower than %.0f%% of runs)",
					job.Duration().Round(time.Second), out.Percentile, out.Percentile),
				Detail: OutlierDetail{
					RunID:        d.Run.ID,
					CommitSHA:    d.Run.HeadSHA,
					Duration:     job.Duration(),
					Percentile:   out.Percentile,
					WorkflowName: k.wf,
					JobName:      job.Name,
				},
			})
		}
	}

	return findings, nil
}

func (o OutlierAnalyzer) detect(data []float64) []stats.OutlierResult {
	switch o.Method {
	case "mad":
		return stats.MADOutliers(data, 3.5)
	default:
		outliers, _ := stats.LogIQROutliers(data, 1.5)
		return outliers
	}
}

func severityFromPercentile(p float64) string {
	switch {
	case p >= 99:
		return "critical"
	case p >= 95:
		return "warning"
	default:
		return "info"
	}
}
