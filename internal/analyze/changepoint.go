package analyze

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/vertti/ci-snitch/internal/stats"
)

// ChangePointDetail contains information about a detected performance shift.
type ChangePointDetail struct {
	WorkflowName string
	JobName      string
	ChangeIdx    int
	BeforeMean   time.Duration
	AfterMean    time.Duration
	PctChange    float64
	Direction    string  // "slowdown" or "speedup"
	PValue       float64 // Mann-Whitney U p-value (< 0.05 = significant)
	CommitSHA    string  // commit at the change point
}

// DetailType implements FindingDetail.
func (ChangePointDetail) DetailType() string { return "changepoint" }

// ChangePointAnalyzer detects when CI performance shifted using CUSUM.
type ChangePointAnalyzer struct {
	// ThresholdMultiplier controls CUSUM sensitivity (default: 4.0)
	ThresholdMultiplier float64
	// MinSegment is the minimum runs between change points (default: 5)
	MinSegment int
}

// Name implements Analyzer.
func (ChangePointAnalyzer) Name() string { return "changepoint" }

// Analyze implements Analyzer.
func (c ChangePointAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) < 10 {
		return nil, nil
	}

	threshold := c.ThresholdMultiplier
	if threshold == 0 {
		threshold = 4.0
	}
	minSeg := c.MinSegment
	if minSeg == 0 {
		minSeg = 5
	}

	// Sort details by time
	sorted := make([]detailRef, len(ac.Details))
	for i := range ac.Details {
		sorted[i] = detailRef{idx: i, created: ac.Details[i].Run.CreatedAt}
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].created.Before(sorted[j].created)
	})

	var findings []Finding

	// Per-(workflow, job) change-point detection.
	// Keying by job name alone would mix distributions from different workflows
	// that happen to share a job name (e.g. "Unit tests").
	type jobKey struct {
		workflow string
		job      string
	}
	type jobSeries struct {
		durations []float64
		refs      []int // indices into sorted
	}
	jobs := make(map[jobKey]*jobSeries)

	for i, ref := range sorted {
		d := ac.Details[ref.idx]
		for _, j := range d.Jobs {
			dur := j.Duration().Seconds()
			if dur <= 0 {
				continue
			}
			k := jobKey{d.Run.WorkflowName, j.Name}
			if jobs[k] == nil {
				jobs[k] = &jobSeries{}
			}
			js := jobs[k]
			js.durations = append(js.durations, dur)
			js.refs = append(js.refs, i)
		}
	}

	for jk, js := range jobs {
		if len(js.durations) < 2*minSeg {
			continue
		}

		cps := stats.CUSUMDetect(js.durations, threshold, minSeg)
		for _, cp := range cps {
			// Find the corresponding run for context
			sortedIdx := js.refs[cp.Index]
			detailIdx := sorted[sortedIdx].idx
			d := ac.Details[detailIdx]

			// Significance test: compare segments before and after.
			// Use all available post-change data (not just minSegment) so the
			// Mann-Whitney test has enough samples for reliable p-values.
			before := js.durations[:cp.Index]
			after := js.durations[cp.Index:]
			_, pValue := stats.MannWhitneyU(before, after)

			severity := classifyChangePoint(pValue, cp.PctChange)

			findings = append(findings, Finding{
				Type:     "changepoint",
				Severity: severity,
				Title:    fmt.Sprintf("Performance %s in job %q", cp.Direction, jk.job),
				Description: fmt.Sprintf("%.0f%% change at %s (commit %s), before: %s, after: %s (p=%.4f)",
					cp.PctChange,
					d.Run.CreatedAt.Format("2006-01-02"),
					truncSHA(d.Run.HeadSHA),
					(time.Duration(cp.BeforeMean * float64(time.Second))).Round(time.Second),
					(time.Duration(cp.AfterMean * float64(time.Second))).Round(time.Second),
					pValue),
				Detail: ChangePointDetail{
					WorkflowName: jk.workflow,
					JobName:      jk.job,
					ChangeIdx:    cp.Index,
					BeforeMean:   time.Duration(cp.BeforeMean * float64(time.Second)),
					AfterMean:    time.Duration(cp.AfterMean * float64(time.Second)),
					PctChange:    cp.PctChange,
					Direction:    cp.Direction,
					PValue:       pValue,
					CommitSHA:    d.Run.HeadSHA,
				},
			})
		}
	}

	return findings, nil
}

type detailRef struct {
	idx     int
	created time.Time
}

func truncSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// classifyChangePoint determines severity based on both statistical significance and effect size.
// Notable if p < 0.05 (significant) or abs(change) >= 15% (large effect).
// Critical requires both.
func classifyChangePoint(pValue, pctChange float64) string {
	significant := pValue < 0.05
	largeEffect := abs(pctChange) >= 15

	switch {
	case significant && largeEffect:
		return "critical"
	case significant || largeEffect:
		return "warning"
	default:
		return "info"
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
