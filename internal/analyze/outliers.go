package analyze

import (
	"context"
	"fmt"
	"time"

	"github.com/vertti/ci-snitch/internal/stats"
)

// OutlierDetail contains information about an outlier run or job.
type OutlierDetail struct {
	RunID        int64         `json:"run_id"`
	CommitSHA    string        `json:"commit_sha"`
	Duration     time.Duration `json:"duration"`
	Percentile   float64       `json:"percentile"`
	WorkflowName string        `json:"workflow_name"`
	JobName      string        `json:"job_name,omitempty"`
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
const (
	minRunsForOutliers = 5
	criticalPercentile = 99.0
	warningPercentile  = 95.0
)

func (o OutlierAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) < minRunsForOutliers {
		return nil, nil
	}

	minPct := o.MinPercentile
	if minPct == 0 {
		minPct = 95
	}

	var findings []Finding

	// Workflow-level outliers
	wfDurations := make(map[int64][]float64)
	wfRuns := make(map[int64][]int) // index into ac.Details
	for i, d := range ac.Details {
		dur := d.Run.Duration().Seconds()
		if dur > 0 {
			wfID := d.Run.WorkflowID
			wfDurations[wfID] = append(wfDurations[wfID], dur)
			wfRuns[wfID] = append(wfRuns[wfID], i)
		}
	}

	for wfID, durations := range wfDurations {
		wfName := ac.WorkflowName(wfID)
		idxMap := wfRuns[wfID]
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
		wfID int64
		job  string
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
				k := jobKey{d.Run.WorkflowID, job.Name}
				jobDurations[k] = append(jobDurations[k], dur)
				jobRefs[k] = append(jobRefs[k], jobRef{i, j})
			}
		}
	}

	for k, durations := range jobDurations {
		wfName := ac.WorkflowName(k.wfID)
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
				Title:    fmt.Sprintf("Slow job %q in %q", job.Name, wfName),
				Description: fmt.Sprintf("Job took %s (p%.0f — slower than %.0f%% of runs)",
					job.Duration().Round(time.Second), out.Percentile, out.Percentile),
				Detail: OutlierDetail{
					RunID:        d.Run.ID,
					CommitSHA:    d.Run.HeadSHA,
					Duration:     job.Duration(),
					Percentile:   out.Percentile,
					WorkflowName: wfName,
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
	case p >= criticalPercentile:
		return SeverityCritical
	case p >= warningPercentile:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}
