package analyze

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
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
	QValue         float64   `json:"q_value"` // BH-adjusted p across all CPs in the analysis
	CommitSHA      string    `json:"commit_sha"`
	Date           time.Time `json:"date"`
	PostChangeRuns int       `json:"post_change_runs"`
	PostChangeCV   float64   `json:"post_change_cv"`
	Persistence    string    `json:"persistence"`
	OverlapRatio   float64   `json:"overlap_ratio"` // fraction of after-points within before-segment's IQR (0-1)
	Category       string    `json:"category,omitempty"`
	// Commit context, enriched post-analysis for regressions (F2/F5):
	CommitFilesChanged int    `json:"commit_files_changed,omitempty"`
	CommitAdditions    int    `json:"commit_additions,omitempty"`
	CommitDeletions    int    `json:"commit_deletions,omitempty"`
	CommitKind         string `json:"commit_kind,omitempty"` // "ci-config" or "code"
}

// DetailType implements FindingDetail.
func (ChangePointDetail) DetailType() string { return TypeChangepoint }

// ChangePointAnalyzer detects when CI performance shifted using CUSUM.
type ChangePointAnalyzer struct {
	// ThresholdMultiplier controls CUSUM sensitivity (default: 4.0)
	ThresholdMultiplier float64
	// MinSegment is the minimum runs between change points (default: 5)
	MinSegment int
}

// Name implements Analyzer.
func (ChangePointAnalyzer) Name() string { return TypeChangepoint }

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

	var findings []Finding

	// Per-(workflow, job) change-point detection over the shared
	// chronological series (see AnalysisContext.JobSeries).
	for jk, js := range ac.JobSeries() {
		wfName := ac.WorkflowName(jk.WorkflowID)
		if len(js.Durations) < 2*minSeg {
			continue
		}

		// Clamp extreme outliers before changepoint detection.
		// A single 38-min run in a 10-min job can fool CUSUM into reporting
		// a persistent regression when the job actually got faster.
		clamped := stats.ClampOutliers(js.Durations, 4.0)
		cps := stats.CUSUMDetect(clamped, threshold, minSeg)
		for cpIdx, cp := range cps {
			// The run where the change surfaced, for commit/date context
			d := ac.Details[js.Refs[cp.Index].DetailIdx]

			// Both segments are bounded by the neighboring change points:
			// comparing the full prefix/suffix would mix levels from other
			// segments (a 5m→8m→5m series would report the speedup against a
			// 6.5m average instead of the 8m plateau) and make the p-value
			// test the wrong hypothesis.
			segStart := 0
			if cpIdx > 0 {
				segStart = cps[cpIdx-1].Index
			}
			postChangeEnd := len(js.Durations)
			if cpIdx+1 < len(cps) {
				postChangeEnd = cps[cpIdx+1].Index
			}
			before := js.Durations[segStart:cp.Index]
			after := js.Durations[cp.Index:postChangeEnd]

			// Significance on raw values: Mann-Whitney is rank-based, so a
			// single outlier has bounded influence. The RNG is seeded from
			// stable inputs so the Monte-Carlo permutation path yields the
			// same p-value on every invocation — unseeded, p-values near the
			// 0.05 boundary drifted ~5% between runs on identical data.
			_, pValue := stats.MannWhitneyURand(before, after, seededRNG(jk.WorkflowID, jk.Job, cp.Index))

			// Segment levels from the clamped series (what CUSUM saw): with
			// bounded segments a raw mean would let one extreme outlier
			// manufacture a large "% change" out of an otherwise flat level.
			beforeMean := stats.Mean(clamped[segStart:cp.Index])
			afterMean := stats.Mean(clamped[cp.Index:postChangeEnd])
			pctChange := 0.0
			if beforeMean != 0 {
				pctChange = (afterMean - beforeMean) / beforeMean * 100
			}
			direction := DirectionSlowdown
			if pctChange < 0 {
				direction = DirectionSpeedup
			}

			// Persistence: how many runs after the change, how stable, did it revert?
			postSegment := after
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
				Type:     TypeChangepoint,
				Severity: severity,
				Title:    fmt.Sprintf("Performance %s in job %q", direction, jk.Job),
				Description: fmt.Sprintf("%.0f%% change at %s (commit %s), before: %s, after: %s (p=%.4f)",
					pctChange,
					d.Run.CreatedAt.Format("2006-01-02"),
					d.Run.HeadSHA[:min(8, len(d.Run.HeadSHA))],
					(time.Duration(beforeMean * float64(time.Second))).Round(time.Second),
					(time.Duration(afterMean * float64(time.Second))).Round(time.Second),
					pValue),
				Detail: ChangePointDetail{
					WorkflowName:   wfName,
					JobName:        jk.Job,
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

// seededRNG returns an RNG deterministically derived from the change point's
// identity, so permutation p-values are reproducible across invocations.
func seededRNG(workflowID int64, jobName string, cpIndex int) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(jobName))
	//nolint:gosec // deterministic seeding for reproducible statistics, not crypto
	return rand.New(rand.NewPCG(uint64(workflowID)^h.Sum64(), uint64(cpIndex)+1))
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
