package analyze

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/vertti/ci-snitch/internal/stats"
)

// OutlierDetail contains information about an outlier run or job.
type OutlierDetail struct {
	RunID        int64    `json:"run_id"`
	CommitSHA    string   `json:"commit_sha"`
	Duration     Duration `json:"duration"`
	Percentile   float64  `json:"percentile"`
	WorkflowName string   `json:"workflow_name"`
	JobName      string   `json:"job_name,omitempty"`
}

// DetailType implements FindingDetail.
func (OutlierDetail) DetailType() string { return TypeOutlier }

// OutlierAnalyzer detects runs or jobs with abnormally long durations.
type OutlierAnalyzer struct {
	// Method selects the outlier detection method: "log-iqr" (default) or "mad"
	Method string
	// MinPercentile is the minimum percentile to report (default: 95).
	// Outliers below this threshold are detected but not emitted as findings.
	MinPercentile float64
}

// Name implements Analyzer.
func (OutlierAnalyzer) Name() string { return TypeOutlier }

const (
	minRunsForOutliers = 5
	criticalPercentile = 99.0
	warningPercentile  = 95.0
)

// Analyze implements Analyzer.
func (o OutlierAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) < minRunsForOutliers {
		return nil, nil
	}

	minPct := o.MinPercentile
	if minPct == 0 {
		minPct = 95
	}

	var findings []Finding

	// Count distinct job names per workflow to skip workflow-level detection
	// for single-job workflows (avoids duplicate entries with job-level detection).
	wfJobNames := make(map[int64]map[string]bool)
	for i := range ac.Details {
		wfID := ac.Details[i].Run.WorkflowID
		if wfJobNames[wfID] == nil {
			wfJobNames[wfID] = make(map[string]bool)
		}
		for j := range ac.Details[i].Jobs {
			wfJobNames[wfID][ac.Details[i].Jobs[j].Name] = true
		}
	}

	// Workflow-level outliers (only for multi-job workflows)
	wfDurations := make(map[int64][]float64)
	wfRuns := make(map[int64][]int) // index into ac.Details
	for i := range ac.Details {
		wfID := ac.Details[i].Run.WorkflowID
		if len(wfJobNames[wfID]) <= 1 {
			continue
		}
		dur := ac.Details[i].Duration().Seconds()
		if dur > 0 {
			wfDurations[wfID] = append(wfDurations[wfID], dur)
			wfRuns[wfID] = append(wfRuns[wfID], i)
		}
	}

	for wfID, durations := range wfDurations {
		wfName := ac.WorkflowName(wfID)
		idxMap := wfRuns[wfID]
		outliers := o.detect(durations)
		minGate := effectiveMinPercentile(minPct, len(durations))
		for _, out := range outliers {
			if out.Percentile < minGate {
				continue
			}
			detailIdx := idxMap[out.Index]
			d := ac.Details[detailIdx]
			findings = append(findings, Finding{
				Type:     TypeOutlier,
				Severity: severityFromPercentile(out.Percentile),
				Title:    fmt.Sprintf("Slow run in %q", wfName),
				Description: fmt.Sprintf("Run took %s (p%.0f — slower than %.0f%% of runs)",
					d.Duration().Round(time.Second), math.Floor(out.Percentile), math.Floor(out.Percentile)),
				Detail: OutlierDetail{
					RunID:        d.Run.ID,
					CommitSHA:    d.Run.HeadSHA,
					Duration:     Duration(d.Duration()),
					Percentile:   out.Percentile,
					WorkflowName: wfName,
				},
			})
		}
	}

	// Job-level outliers over the shared per-(workflow, job) series
	for k, js := range ac.JobSeries() {
		wfName := ac.WorkflowName(k.WorkflowID)
		outliers := o.detect(js.Durations)
		minGate := effectiveMinPercentile(minPct, len(js.Durations))
		for _, out := range outliers {
			if out.Percentile < minGate {
				continue
			}
			ref := js.Refs[out.Index]
			d := ac.Details[ref.DetailIdx]
			job := d.Jobs[ref.JobIdx]
			findings = append(findings, Finding{
				Type:     TypeOutlier,
				Severity: severityFromPercentile(out.Percentile),
				Title:    fmt.Sprintf("Slow job %q in %q", job.Name, wfName),
				Description: fmt.Sprintf("Job took %s (p%.0f — slower than %.0f%% of runs)",
					job.Duration().Round(time.Second), math.Floor(out.Percentile), math.Floor(out.Percentile)),
				Detail: OutlierDetail{
					RunID:        d.Run.ID,
					CommitSHA:    d.Run.HeadSHA,
					Duration:     Duration(job.Duration()),
					Percentile:   out.Percentile,
					WorkflowName: wfName,
					JobName:      job.Name,
				},
			})
		}
	}

	return findings, nil
}

// effectiveMinPercentile caps the reporting gate at the highest percentile a
// sample of size n can produce (the maximum midranks at (n-0.5)/n). Without
// the cap, a fixed p95 gate structurally suppresses every fence-detected
// outlier in series shorter than 10 runs.
func effectiveMinPercentile(minPct float64, n int) float64 {
	maxAchievable := (float64(n) - 0.5) / float64(n) * 100
	return math.Min(minPct, maxAchievable)
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
