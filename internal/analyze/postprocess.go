package analyze

import (
	"fmt"
	"time"

	"github.com/vertti/ci-snitch/internal/stats"
)

// postProcess transforms raw findings from analyzers into curated findings
// that all formatters can render without reimplementing filtering logic.
func postProcess(findings []Finding) []Finding {
	var result []Finding
	for _, f := range findings {
		switch f.Type {
		case TypeChangepoint:
			// Handled in batch below
		case TypeOutlier:
			// Handled in batch below
		case TypeFailure:
			d, ok := f.Detail.(FailureDetail)
			if ok && d.FailureRate < warningFailureRate && d.CancellationRate < warningFailureRate {
				continue // drop workflows with negligible failure and cancellation rates
			}
			result = append(result, f)
		default:
			result = append(result, f)
		}
	}

	result = append(result, categorizeChangePoints(findings)...)
	result = append(result, groupOutliers(findings)...)

	return result
}

// categorizeChangePoints assigns a Category to each change point:
// - oscillating: job has 3+ notable change points (too volatile)
// - regression: latest non-transient slowdown per (workflow, job) (deduplicated)
// - speedup: improvements
// - minor: severity=info
//
// Jobs routinely share names across workflows ("build", "test"), so both
// the oscillation count and the regression dedup key on (workflow, job) —
// the same identity the analyzer itself uses.
func categorizeChangePoints(findings []Finding) []Finding {
	var changepoints []Finding
	for _, f := range findings {
		if f.Type == TypeChangepoint {
			changepoints = append(changepoints, f)
		}
	}

	applyMultipleComparisonCorrection(changepoints)

	// Count notable change points per (workflow, job)
	jobCounts := make(map[wfJobName]int)
	for _, f := range changepoints {
		if f.Severity == SeverityInfo {
			continue
		}
		d, ok := f.Detail.(ChangePointDetail)
		if !ok {
			continue
		}
		jobCounts[wfJobName{d.WorkflowName, d.JobName}]++
	}

	// Track latest regression per (workflow, job) for dedup
	latestRegression := make(map[wfJobName]int) // (workflow, job) -> index in result

	var result []Finding
	for _, f := range changepoints {
		d, ok := f.Detail.(ChangePointDetail)
		if !ok {
			result = append(result, f)
			continue
		}
		key := wfJobName{d.WorkflowName, d.JobName}

		switch {
		case f.Severity == SeverityInfo:
			d.Category = CategoryMinor
		case jobCounts[key] >= 3:
			d.Category = CategoryOscillating
		case d.OverlapRatio > 0.5:
			// >50% of after-points fall within the before-segment's IQR.
			// The "shift" is likely driven by outliers, not a level change.
			d.Category = CategoryMinor
		case d.Direction == DirectionSpeedup:
			d.Category = CategorySpeedup
		default:
			d.Category = CategoryRegression
		}

		f.Detail = d
		result = append(result, f)

		// Track latest regression per (workflow, job)
		if d.Category == CategoryRegression && d.Direction == DirectionSlowdown && d.Persistence != PersistenceTransient {
			if idx, exists := latestRegression[key]; exists {
				// Mark the older one as minor
				older := result[idx]
				od, _ := older.Detail.(ChangePointDetail)
				od.Category = CategoryMinor
				result[idx].Detail = od
			}
			latestRegression[key] = len(result) - 1
		}
	}

	return result
}

// applyMultipleComparisonCorrection converts the change points' p-values into
// Benjamini-Hochberg q-values and demotes findings whose q exceeds alpha to
// info severity. One Mann-Whitney test per change point across dozens of jobs
// at alpha=0.05 would otherwise surface ~1 false "significant regression"
// per 20 stable jobs. Runs before oscillation counting and regression dedup
// so statistically insignificant shifts feed neither.
func applyMultipleComparisonCorrection(changepoints []Finding) {
	var ps []float64
	var idxs []int
	for i := range changepoints {
		if d, ok := changepoints[i].Detail.(ChangePointDetail); ok {
			ps = append(ps, d.PValue)
			idxs = append(idxs, i)
		}
	}
	qs := stats.BenjaminiHochberg(ps)
	for j, i := range idxs {
		d, ok := changepoints[i].Detail.(ChangePointDetail)
		if !ok {
			continue
		}
		d.QValue = qs[j]
		changepoints[i].Detail = d
		if changepoints[i].Severity != SeverityInfo && d.QValue > significanceAlpha {
			changepoints[i].Severity = SeverityInfo
		}
	}
}

// groupOutliers collapses individual outlier findings into one finding per (workflow, job).
func groupOutliers(findings []Finding) []Finding {
	type group struct {
		key         wfJobName
		count       int
		worstDur    Duration
		worstPct    float64
		worstCommit string
		maxSeverity string
	}

	groups := make(map[wfJobName]*group)
	var order []wfJobName
	for _, f := range findings {
		if f.Type != TypeOutlier {
			continue
		}
		d, ok := f.Detail.(OutlierDetail)
		if !ok {
			continue
		}
		k := wfJobName{d.WorkflowName, d.JobName}
		g, ok := groups[k]
		if !ok {
			g = &group{key: k, maxSeverity: SeverityInfo}
			groups[k] = g
			order = append(order, k)
		}
		g.count++
		if d.Duration > g.worstDur {
			g.worstDur = d.Duration
			g.worstPct = d.Percentile
			g.worstCommit = d.CommitSHA
		}
		if f.Severity == SeverityCritical || (f.Severity == SeverityWarning && g.maxSeverity != SeverityCritical) {
			g.maxSeverity = f.Severity
		}
	}

	// Collapse job-level groups that duplicate a workflow-level group
	// (same count, same worst commit). When an incident affects all matrix
	// variants, the workflow-level row already tells the story.
	wfGroups := make(map[string]*group)
	for _, k := range order {
		if k.job == "" {
			wfGroups[k.wf] = groups[k]
		}
	}
	isDuplicate := func(k wfJobName) bool {
		if k.job == "" {
			return false
		}
		wfg := wfGroups[k.wf]
		if wfg == nil {
			return false
		}
		return wfg.count == groups[k].count && wfg.worstCommit == groups[k].worstCommit
	}

	var result []Finding
	for _, k := range order {
		if isDuplicate(k) {
			continue
		}
		g := groups[k]
		subject := k.wf
		if k.job != "" {
			subject += " / " + k.job
		}
		result = append(result, Finding{
			Type:     TypeOutlier,
			Severity: g.maxSeverity,
			Title:    "Outliers in " + subject,
			Description: fmt.Sprintf("%dx, worst %s (p%.0f)",
				g.count, g.worstDur.Round(time.Second), g.worstPct),
			Detail: OutlierGroupDetail{
				WorkflowName:    k.wf,
				JobName:         k.job,
				Count:           g.count,
				WorstDuration:   g.worstDur,
				WorstPercentile: g.worstPct,
				WorstCommitSHA:  g.worstCommit,
				MaxSeverity:     g.maxSeverity,
			},
		})
	}

	return result
}
