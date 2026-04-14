package output

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// LLMFormatter produces structured output optimized for LLM consumption.
// Combines narrative context with raw JSON data.
type LLMFormatter struct{}

// Format implements Formatter.
func (LLMFormatter) Format(w io.Writer, result analyze.AnalysisResult) error {
	// Header
	_, _ = fmt.Fprintf(w, "# CI Analysis (%d runs, %s to %s)\n\n",
		result.Meta.TotalRuns,
		result.Meta.TimeRange[0].Format("2006-01-02"),
		result.Meta.TimeRange[1].Format("2006-01-02"))

	// Group findings
	var summaries, outliers, changepoints, failures, costs []analyze.Finding
	for _, f := range result.Findings {
		switch f.Type {
		case analyze.TypeSummary:
			summaries = append(summaries, f)
		case analyze.TypeOutlier:
			outliers = append(outliers, f)
		case analyze.TypeChangepoint:
			changepoints = append(changepoints, f)
		case analyze.TypeFailure:
			failures = append(failures, f)
		case analyze.TypeCost:
			costs = append(costs, f)
		}
	}

	// Priority findings
	_, _ = fmt.Fprint(w, "## Priority Findings\n\n")
	hasPriority := false

	for _, f := range changepoints {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok || d.Category != analyze.CategoryRegression || d.Direction != analyze.DirectionSlowdown {
			continue
		}
		hasPriority = true
		_, _ = fmt.Fprintf(w, "- **[REGRESSION]** %s: %+.0f%% (%s -> %s) at commit `%s` on %s",
			d.JobName, d.PctChange,
			fmtDur(d.BeforeMean), fmtDur(d.AfterMean),
			truncSHA(d.CommitSHA), d.Date.Format("2006-01-02"))
		if d.Persistence == analyze.PersistencePersistent {
			_, _ = fmt.Fprintf(w, " -- persistent over %d runs", d.PostChangeRuns)
		}
		_, _ = fmt.Fprintf(w, " (p=%.4f)\n", d.PValue)
	}

	for _, f := range failures {
		d, ok := f.Detail.(analyze.FailureDetail)
		if !ok {
			continue
		}
		hasPriority = true
		_, _ = fmt.Fprintf(w, "- **[FLAKY]** %s: %.0f%% failure rate (%d/%d runs)",
			d.Workflow, d.FailureRate*100, d.FailureCount, d.TotalRuns)
		if d.RetriedRuns > 0 {
			_, _ = fmt.Fprintf(w, ", %d retried (+%d extra attempts)", d.RetriedRuns, d.ExtraAttempts)
		}
		_, _ = fmt.Fprint(w, "\n")
	}

	costLimit := min(3, len(costs))
	for i := range costLimit {
		d, ok := costs[i].Detail.(analyze.CostDetail)
		if !ok {
			continue
		}
		hasPriority = true
		_, _ = fmt.Fprintf(w, "- **[COST]** %s: %.0f billable mins/day (%.0f total)",
			d.Workflow, d.DailyRate, d.BillableMinutes)
		if d.DailySavingsEstimate > 0 {
			_, _ = fmt.Fprintf(w, " -- potential savings ~%.0f mins/day", d.DailySavingsEstimate)
		}
		_, _ = fmt.Fprint(w, "\n")
	}

	if !hasPriority {
		_, _ = fmt.Fprint(w, "No critical findings.\n")
	}

	// Workflow summaries
	_, _ = fmt.Fprint(w, "\n## Workflow Summaries\n\n")
	_, _ = fmt.Fprint(w, "| Workflow | Runs | Median | P95 | Total | Volatility |\n")
	_, _ = fmt.Fprint(w, "|----------|------|--------|-----|-------|------------|\n")
	for _, f := range summaries {
		d, ok := f.Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "| %s | %d | %s | %s | %s | %s |\n",
			d.Workflow, d.Stats.TotalRuns,
			fmtDur(d.Stats.Median), fmtDur(d.Stats.P95),
			fmtTotalTime(d.Stats.TotalTime), d.Stats.VolatilityLabel)
	}

	// Suggested investigations
	suggestions := buildSuggestions(changepoints, failures, costs, outliers)
	if len(suggestions) > 0 {
		_, _ = fmt.Fprint(w, "\n## Suggested Investigations\n\n")
		for _, s := range suggestions {
			_, _ = fmt.Fprintf(w, "- %s\n", s)
		}
	}

	// Raw JSON
	_, _ = fmt.Fprint(w, "\n## Raw Data\n\n```json\n")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	_, _ = fmt.Fprint(w, "```\n")

	return nil
}

func buildSuggestions(changepoints, failures, costs, outliers []analyze.Finding) []string {
	var suggestions []string

	for _, f := range changepoints {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok || d.Category != analyze.CategoryRegression || d.Direction != analyze.DirectionSlowdown {
			continue
		}
		suggestions = append(suggestions,
			fmt.Sprintf("What changed in commit `%s` (%s) that affected %q?",
				truncSHA(d.CommitSHA), d.Date.Format("2006-01-02"), d.JobName))
	}

	for _, f := range costs {
		d, ok := f.Detail.(analyze.CostDetail)
		if !ok || d.DailySavingsEstimate < 10 {
			continue
		}
		suggestions = append(suggestions,
			fmt.Sprintf("%q has high variance (save ~%.0f mins/day if stabilized) -- check for flaky tests, cache misses, or resource contention",
				d.Workflow, d.DailySavingsEstimate))
	}

	// Frequent outlier groups
	for _, f := range outliers {
		d, ok := f.Detail.(analyze.OutlierGroupDetail)
		if !ok || d.Count < 5 {
			continue
		}
		subject := d.WorkflowName
		if d.JobName != "" {
			subject = d.JobName
		}
		suggestions = append(suggestions,
			fmt.Sprintf("%q has %d outliers (worst %s) -- check for resource contention or flaky infrastructure",
				subject, d.Count, fmtDur(d.WorstDuration)))
	}

	for _, f := range failures {
		d, ok := f.Detail.(analyze.FailureDetail)
		if !ok || d.FailureRate < 0.1 {
			continue
		}
		var conclusionHint string
		maxConclusion := ""
		maxCount := 0
		for c, n := range d.ByConclusion {
			if n > maxCount {
				maxCount = n
				maxConclusion = c
			}
		}
		switch maxConclusion {
		case "cancelled":
			conclusionHint = " (mostly cancelled -- check for timeout issues or manual cancellations)"
		case "timed_out":
			conclusionHint = " (timed out -- likely hanging test or resource exhaustion)"
		}
		suggestions = append(suggestions,
			fmt.Sprintf("%q has %.0f%% failure rate%s",
				d.Workflow, d.FailureRate*100, conclusionHint))
	}

	return suggestions
}
