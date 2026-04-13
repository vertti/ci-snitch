package analyze

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/vertti/ci-snitch/internal/stats"
)

// ChangePointDetail contains information about a detected performance shift.
type ChangePointDetail struct {
	WorkflowName   string        `json:"workflow_name"`
	JobName        string        `json:"job_name"`
	ChangeIdx      int           `json:"change_idx"`
	BeforeMean     time.Duration `json:"before_mean"`
	AfterMean      time.Duration `json:"after_mean"`
	PctChange      float64       `json:"pct_change"`
	Direction      string        `json:"direction"`
	PValue         float64       `json:"p_value"`
	CommitSHA      string        `json:"commit_sha"`
	Date           time.Time     `json:"date"`
	PostChangeRuns int           `json:"post_change_runs"`
	PostChangeCV   float64       `json:"post_change_cv"`
	Persistence    string        `json:"persistence"`
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
const (
	minRunsForChangePoint = 10
	significanceAlpha     = 0.05
	highSignificanceAlpha = 0.01
	largeEffectPct        = 20.0
	meaningfulEffectPct   = 10.0
)

func (c ChangePointAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) < minRunsForChangePoint {
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
		wfID int64
		job  string
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
			k := jobKey{d.Run.WorkflowID, j.Name}
			if jobs[k] == nil {
				jobs[k] = &jobSeries{}
			}
			js := jobs[k]
			js.durations = append(js.durations, dur)
			js.refs = append(js.refs, i)
		}
	}

	for jk, js := range jobs {
		wfName := ac.WorkflowName(jk.wfID)
		if len(js.durations) < 2*minSeg {
			continue
		}

		cps := stats.CUSUMDetect(js.durations, threshold, minSeg)
		for cpIdx, cp := range cps {
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

			// Persistence: how many runs after the change, how stable, did it revert?
			postChangeEnd := len(js.durations)
			if cpIdx+1 < len(cps) {
				postChangeEnd = cps[cpIdx+1].Index
			}
			postSegment := js.durations[cp.Index:postChangeEnd]
			postChangeRuns := len(postSegment)
			postChangeCV := coefficientOfVariation(postSegment)
			persistence := classifyPersistence(postChangeRuns, minSeg, cps, cpIdx)

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
					WorkflowName:   wfName,
					JobName:        jk.job,
					ChangeIdx:      cp.Index,
					BeforeMean:     time.Duration(cp.BeforeMean * float64(time.Second)),
					AfterMean:      time.Duration(cp.AfterMean * float64(time.Second)),
					PctChange:      cp.PctChange,
					Direction:      cp.Direction,
					PValue:         pValue,
					CommitSHA:      d.Run.HeadSHA,
					Date:           d.Run.CreatedAt,
					PostChangeRuns: postChangeRuns,
					PostChangeCV:   postChangeCV,
					Persistence:    persistence,
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

func coefficientOfVariation(data []float64) float64 {
	m := stats.Mean(data)
	if m == 0 {
		return 0
	}
	return stats.Stddev(data) / m
}

// classifyPersistence determines whether a change point is persistent, transient, or inconclusive.
//   - persistent: enough post-change runs and no subsequent revert detected
//   - transient: a subsequent change point reverses the direction
//   - inconclusive: too few post-change runs to tell
func classifyPersistence(postChangeRuns, minSeg int, cps []stats.ChangePoint, cpIdx int) string {
	if postChangeRuns < 2*minSeg {
		return "inconclusive"
	}
	// If there's a next change point that reverses direction, it's transient.
	if cpIdx+1 < len(cps) {
		current := cps[cpIdx]
		next := cps[cpIdx+1]
		if current.Direction != next.Direction {
			return "transient"
		}
	}
	return "persistent"
}

// classifyChangePoint determines severity based on statistical significance and effect size.
// Critical: p < 0.01 and abs(change) >= 20%.
// Warning (notable): p < 0.05 and abs(change) >= 10%.
// Info (minor): everything else -- shown only in verbose mode.
func classifyChangePoint(pValue, pctChange float64) string {
	significant := pValue < significanceAlpha
	largeEffect := math.Abs(pctChange) >= largeEffectPct
	meaningfulEffect := math.Abs(pctChange) >= meaningfulEffectPct

	switch {
	case pValue < highSignificanceAlpha && largeEffect:
		return SeverityCritical
	case significant && meaningfulEffect:
		return SeverityWarning
	default:
		return SeverityInfo
	}
}
