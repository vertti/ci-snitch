package analyze

import (
	"fmt"
	"time"
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
			if ok && d.FailureRate < 0.05 {
				continue // drop sub-5% failure rates
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
// - regression: latest non-transient slowdown per job (deduplicated)
// - speedup: improvements
// - minor: severity=info
func categorizeChangePoints(findings []Finding) []Finding {
	var changepoints []Finding
	for _, f := range findings {
		if f.Type == TypeChangepoint {
			changepoints = append(changepoints, f)
		}
	}

	// Count notable change points per job
	jobCounts := make(map[string]int)
	for _, f := range changepoints {
		if f.Severity == SeverityInfo {
			continue
		}
		d, ok := f.Detail.(ChangePointDetail)
		if !ok {
			continue
		}
		jobCounts[d.JobName]++
	}

	// Track latest regression per job for dedup
	latestRegression := make(map[string]int) // job -> index in result

	var result []Finding
	for _, f := range changepoints {
		d, ok := f.Detail.(ChangePointDetail)
		if !ok {
			result = append(result, f)
			continue
		}

		switch {
		case f.Severity == SeverityInfo:
			d.Category = CategoryMinor
		case jobCounts[d.JobName] >= 3:
			d.Category = CategoryOscillating
		case d.Direction == DirectionSpeedup:
			d.Category = CategorySpeedup
		default:
			d.Category = CategoryRegression
		}

		f.Detail = d
		result = append(result, f)

		// Track latest regression per job
		if d.Category == CategoryRegression && d.Direction == DirectionSlowdown && d.Persistence != PersistenceTransient {
			if idx, exists := latestRegression[d.JobName]; exists {
				// Mark the older one as minor
				older := result[idx]
				od, _ := older.Detail.(ChangePointDetail)
				od.Category = CategoryMinor
				result[idx].Detail = od
			}
			latestRegression[d.JobName] = len(result) - 1
		}
	}

	return result
}

// groupOutliers collapses individual outlier findings into one finding per (workflow, job).
func groupOutliers(findings []Finding) []Finding {
	type groupKey struct{ wf, job string }
	type group struct {
		key         groupKey
		count       int
		worstDur    Duration
		worstPct    float64
		worstCommit string
		maxSeverity string
	}

	groups := make(map[groupKey]*group)
	var order []groupKey
	for _, f := range findings {
		if f.Type != TypeOutlier {
			continue
		}
		d, ok := f.Detail.(OutlierDetail)
		if !ok {
			continue
		}
		k := groupKey{d.WorkflowName, d.JobName}
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

	var result []Finding
	for _, k := range order {
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
