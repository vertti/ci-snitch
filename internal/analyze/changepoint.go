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
	WorkflowName   string    `json:"workflow_name"`
	JobName        string    `json:"job_name"`
	ChangeIdx      int       `json:"change_idx"`
	BeforeMean     Duration  `json:"before_mean"`
	AfterMean      Duration  `json:"after_mean"`
	PctChange      float64   `json:"pct_change"`
	Direction      string    `json:"direction"`
	PValue         float64   `json:"p_value"`
	CommitSHA      string    `json:"commit_sha"`
	Date           time.Time `json:"date"`
	PostChangeRuns int       `json:"post_change_runs"`
	PostChangeCV   float64   `json:"post_change_cv"`
	Persistence    string    `json:"persistence"`
	OverlapRatio   float64   `json:"overlap_ratio"` // fraction of after-points within before-segment's IQR (0-1)
	Category       string    `json:"category,omitempty"`
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
	// minAbsDeltaSeconds is the minimum absolute change in seconds for a
	// changepoint to be considered notable. A 5s→6s job is 20% slower but
	// not worth investigating.
	minAbsDeltaSeconds = 10.0
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
		for j := range d.Jobs {
			dur := d.Jobs[j].Duration().Seconds()
			if dur <= 0 {
				continue
			}
			k := jobKey{d.Run.WorkflowID, d.Jobs[j].Name}
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

		// Clamp extreme outliers before changepoint detection.
		// A single 38-min run in a 10-min job can fool CUSUM into reporting
		// a persistent regression when the job actually got faster.
		clamped := stats.ClampOutliers(js.durations, 4.0)
		cps := stats.CUSUMDetect(clamped, threshold, minSeg)
		for cpIdx, cp := range cps {
			// Find the corresponding run for context
			sortedIdx := js.refs[cp.Index]
			detailIdx := sorted[sortedIdx].idx
			d := ac.Details[detailIdx]

			// Use raw (unclamped) durations for significance testing and reporting.
			// CUSUM detects the change point index on clamped data; we verify
			// and report using the original values.
			before := js.durations[:cp.Index]
			after := js.durations[cp.Index:]
			_, pValue := stats.MannWhitneyU(before, after)

			// Recompute means from raw data for accurate reporting
			beforeMean := stats.Mean(before)
			afterMean := stats.Mean(after)
			pctChange := 0.0
			if beforeMean != 0 {
				pctChange = (afterMean - beforeMean) / beforeMean * 100
			}
			direction := DirectionSlowdown
			if pctChange < 0 {
				direction = DirectionSpeedup
			}

			// Persistence: how many runs after the change, how stable, did it revert?
			postChangeEnd := len(js.durations)
			if cpIdx+1 < len(cps) {
				postChangeEnd = cps[cpIdx+1].Index
			}
			postSegment := js.durations[cp.Index:postChangeEnd]
			postChangeRuns := len(postSegment)
			postChangeCV := coefficientOfVariation(postSegment)
			persistence := classifyPersistence(postChangeRuns, minSeg, cps, cpIdx)

			// Overlap ratio: what fraction of after-points fall within the
			// before-segment's IQR? High overlap suggests the "shift" is
			// driven by a few outliers, not a genuine level change.
			overlapRatio := computeOverlapRatio(before, after)

			absDelta := math.Abs(afterMean - beforeMean)
			severity := classifyChangePoint(pValue, pctChange, absDelta)

			findings = append(findings, Finding{
				Type:     "changepoint",
				Severity: severity,
				Title:    fmt.Sprintf("Performance %s in job %q", direction, jk.job),
				Description: fmt.Sprintf("%.0f%% change at %s (commit %s), before: %s, after: %s (p=%.4f)",
					pctChange,
					d.Run.CreatedAt.Format("2006-01-02"),
					d.Run.HeadSHA[:min(8, len(d.Run.HeadSHA))],
					(time.Duration(beforeMean * float64(time.Second))).Round(time.Second),
					(time.Duration(afterMean * float64(time.Second))).Round(time.Second),
					pValue),
				Detail: ChangePointDetail{
					WorkflowName:   wfName,
					JobName:        jk.job,
					ChangeIdx:      cp.Index,
					BeforeMean:     Duration(beforeMean * float64(time.Second)),
					AfterMean:      Duration(afterMean * float64(time.Second)),
					PctChange:      pctChange,
					Direction:      direction,
					PValue:         pValue,
					CommitSHA:      d.Run.HeadSHA,
					Date:           d.Run.CreatedAt,
					PostChangeRuns: postChangeRuns,
					PostChangeCV:   postChangeCV,
					Persistence:    persistence,
					OverlapRatio:   overlapRatio,
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

// computeOverlapRatio returns the fraction of after-segment points that fall
// within the before-segment's IQR (Q1 to Q3). A high ratio (>0.5) suggests
// the detected shift is driven by outliers, not a genuine level change.
func computeOverlapRatio(before, after []float64) float64 {
	if len(before) < 4 || len(after) == 0 {
		return 0
	}
	q1, q3, _ := stats.IQR(before)
	count := 0
	for _, v := range after {
		if v >= q1 && v <= q3 {
			count++
		}
	}
	return float64(count) / float64(len(after))
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

// classifyChangePoint determines severity based on statistical significance, effect size,
// and absolute duration delta. A 5s→6s job is 20% slower but not worth alerting on.
func classifyChangePoint(pValue, pctChange, absDeltaSeconds float64) string {
	if absDeltaSeconds < minAbsDeltaSeconds {
		return SeverityInfo
	}

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
