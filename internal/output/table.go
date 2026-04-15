package output

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// TableFormatter outputs results as a human-readable table.
type TableFormatter struct {
	Verbose bool
}

// Format implements Formatter.
func (t TableFormatter) Format(w io.Writer, result analyze.AnalysisResult) error {
	if len(result.Findings) == 0 {
		_, err := fmt.Fprintln(w, "No findings.")
		return err
	}

	g := groupByType(result.Findings)

	if len(g.Summaries) > 0 {
		writeTriageHeader(w, g.Summaries, g.Changepoints, g.Failures)
		if err := writeSummaryTable(w, g.Summaries); err != nil {
			return err
		}
	}

	if len(g.Steps) > 0 {
		writeStepTable(w, g.Steps)
	}

	if len(g.Costs) > 0 {
		writeCostTable(w, g.Costs)
	}

	if len(g.Failures) > 0 {
		writeFailureTable(w, g.Failures)
	}

	if len(g.Outliers) > 0 {
		if err := writeOutlierTable(w, g.Outliers); err != nil {
			return err
		}
	}

	if len(g.Changepoints) > 0 {
		if err := writeChangePointTable(w, g.Changepoints, t.Verbose); err != nil {
			return err
		}
	}

	// Meta
	_, _ = fmt.Fprintf(w, "\n%s%d runs analyzed%s (%s to %s)\n",
		dim, result.Meta.TotalRuns, reset,
		result.Meta.TimeRange[0].Format("2006-01-02"),
		result.Meta.TimeRange[1].Format("2006-01-02"))

	// Legend
	writeLegend(w)
	return nil
}

// ANSI color codes
const (
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	reset  = "\033[0m"
)

func writeTriageHeader(w io.Writer, summaries, changepoints, failures []analyze.Finding) {
	_, _ = fmt.Fprintf(w, "%s── Triage ──%s\n", dim, reset)

	// Top 3 by total CI time (summaries are already sorted)
	_, _ = fmt.Fprintf(w, "  %sTop CI time:%s  ", dim, reset)
	count := min(3, len(summaries))
	for i := range count {
		d, ok := summaries[i].Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		if i > 0 {
			_, _ = fmt.Fprint(w, "  ")
		}
		_, _ = fmt.Fprintf(w, "%s%s%s %s(%s)%s", bold, d.Workflow, reset, dim, fmtTotalTime(d.Stats.TotalTime), reset)
	}
	_, _ = fmt.Fprintln(w)

	// Most volatile workflows
	var volatile []string
	for _, f := range summaries {
		d, ok := f.Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		if d.Stats.VolatilityLabel == "volatile" || d.Stats.VolatilityLabel == "spiky" {
			volatile = append(volatile, d.Workflow)
		}
	}
	if len(volatile) > 0 {
		_, _ = fmt.Fprintf(w, "  %sUnpredictable:%s  ", dim, reset)
		for i, name := range volatile {
			if i > 0 {
				_, _ = fmt.Fprint(w, ", ")
			}
			_, _ = fmt.Fprintf(w, "%s%s%s", yellow, name, reset)
		}
		_, _ = fmt.Fprintln(w)
	}

	// Active regressions (already deduplicated by postprocessor)
	var regressions []analyze.ChangePointDetail
	for _, f := range changepoints {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if ok && d.Category == analyze.CategoryRegression && d.Direction == analyze.DirectionSlowdown {
			regressions = append(regressions, d)
		}
	}
	if len(regressions) > 0 {
		_, _ = fmt.Fprintf(w, "  %sRegressions:%s  ", dim, reset)
		shown := min(5, len(regressions))
		for i, d := range regressions[:shown] {
			if i > 0 {
				_, _ = fmt.Fprint(w, ", ")
			}
			_, _ = fmt.Fprintf(w, "%s%s %+.0f%%%s", red, d.JobName, d.PctChange, reset)
		}
		if len(regressions) > shown {
			_, _ = fmt.Fprintf(w, "%s, +%d more%s", dim, len(regressions)-shown, reset)
		}
		_, _ = fmt.Fprintln(w)
	}

	// Flaky workflows
	if len(failures) > 0 {
		_, _ = fmt.Fprintf(w, "  %sFlaky:%s  ", dim, reset)
		count := min(3, len(failures))
		for i := range count {
			d, ok := failures[i].Detail.(analyze.FailureDetail)
			if !ok {
				continue
			}
			if i > 0 {
				_, _ = fmt.Fprint(w, ", ")
			}
			_, _ = fmt.Fprintf(w, "%s%s%s %s(%.0f%%)%s", red, d.Workflow, reset, dim, d.FailureRate*100, reset)
		}
		_, _ = fmt.Fprintln(w)
	}

	_, _ = fmt.Fprintln(w)
}

func writeSummaryTable(w io.Writer, findings []analyze.Finding) error {
	// Findings are already sorted by total CI time descending from the analyzer.
	// Split into multi-job and single-job workflows so each group gets its own
	// tabwriter context -- prevents a long name in one group from blowing up
	// column widths in the other.
	var multiJob, singleJob []indexedFinding
	for i, f := range findings {
		d, ok := f.Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		// Workflows with ≤2 runs: don't expand job tree (stats are meaningless)
		if len(d.Jobs) > 1 && d.Stats.TotalRuns > 2 {
			multiJob = append(multiJob, indexedFinding{i, f})
		} else {
			singleJob = append(singleJob, indexedFinding{i, f})
		}
	}

	firstIdx := 0
	if len(multiJob) > 0 {
		firstIdx = multiJob[0].idx
	} else if len(singleJob) > 0 {
		firstIdx = singleJob[0].idx
	}

	// Multi-job workflows: each gets its own tabwriter for the job tree.
	for _, mf := range multiJob {
		d, _ := mf.finding.Detail.(analyze.SummaryDetail)
		marker := mostCITimeMarker(mf.idx, firstIdx, len(findings))
		volTag := fmtVolatility(d.Stats.VolatilityLabel)
		queueTag := fmtQueueTime(d.Queue)

		_, _ = fmt.Fprintf(w, "%s%s%s  %d runs, median %s%s%s, p95 %s%s%s, total %s%s%s%s%s%s\n",
			bold, d.Workflow, reset,
			d.Stats.TotalRuns,
			cyan, fmtDur(d.Stats.Median), reset,
			cyan, fmtDur(d.Stats.P95), reset,
			bold, fmtTotalTime(d.Stats.TotalTime), reset,
			volTag, queueTag, marker)

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for j, job := range d.Jobs {
			prefix := "  |-"
			if j == len(d.Jobs)-1 {
				prefix = "  `-"
			}
			jobVol := fmtVolatility(job.Stats.VolatilityLabel)
			_, _ = fmt.Fprintf(tw, "%s%s%s %s\t%d runs\tmedian %s\tp95 %s\tmin %s\tmax %s%s\n",
				dim, prefix, reset, job.Name,
				job.Stats.TotalRuns,
				fmtDur(job.Stats.Median), fmtDur(job.Stats.P95),
				fmtDur(job.Stats.Min), fmtDur(job.Stats.Max),
				jobVol)
		}
		_ = tw.Flush()
		_, _ = fmt.Fprintln(w)
	}

	// Single-job workflows: aligned together in one tabwriter block.
	if len(singleJob) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, sf := range singleJob {
			d, _ := sf.finding.Detail.(analyze.SummaryDetail)
			marker := mostCITimeMarker(sf.idx, firstIdx, len(findings))
			volTag := fmtVolatility(d.Stats.VolatilityLabel)
			queueTag := fmtQueueTime(d.Queue)

			_, _ = fmt.Fprintf(tw, "%s%s%s\t%d runs\tmedian %s%s%s\tp95 %s%s%s\ttotal %s%s%s%s%s%s\n",
				bold, d.Workflow, reset,
				d.Stats.TotalRuns,
				cyan, fmtDur(d.Stats.Median), reset,
				cyan, fmtDur(d.Stats.P95), reset,
				bold, fmtTotalTime(d.Stats.TotalTime), reset,
				volTag, queueTag, marker)
		}
		_ = tw.Flush()
		_, _ = fmt.Fprintln(w)
	}

	return nil
}

type indexedFinding struct {
	idx     int
	finding analyze.Finding
}

func mostCITimeMarker(idx, firstIdx, total int) string {
	if idx == firstIdx && total > 1 {
		return red + " << most CI time" + reset
	}
	return ""
}

func fmtVolatility(label string) string {
	switch label {
	case "volatile":
		return " " + red + "[volatile]" + reset
	case "spiky":
		return " " + yellow + "[spiky]" + reset
	case "variable":
		return " " + dim + "[variable]" + reset
	default:
		return ""
	}
}

func fmtQueueTime(q analyze.QueueStats) string {
	// Only show queue time when median is notable (> 5 seconds)
	if q.Median.Std() <= 5*time.Second {
		return ""
	}
	return fmt.Sprintf(" %s[queue %s]%s", yellow, fmtDur(q.Median), reset)
}

func writeStepTable(w io.Writer, findings []analyze.Finding) {
	shown := min(5, len(findings))
	_, _ = fmt.Fprintf(w, "%s── Step Breakdown (top %d jobs) ──%s\n", dim, shown, reset)

	for _, f := range findings[:shown] {
		d, ok := f.Detail.(analyze.StepTimingDetail)
		if !ok {
			continue
		}

		_, _ = fmt.Fprintf(w, "  %s%s%s %s/ %s%s\n",
			bold, d.WorkflowName, reset, dim, d.JobName, reset)

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, st := range d.Steps {
			volTag := ""
			if st.Volatility >= 2.0 {
				volTag = fmt.Sprintf(" %s[%.1fx]%s", yellow, st.Volatility, reset)
			}
			_, _ = fmt.Fprintf(tw, "    %s\tmedian %s\tp95 %s\t%s%.0f%% of job%s%s\n",
				st.Name,
				fmtDur(st.Median), fmtDur(st.P95),
				dim, st.PctOfJob, reset,
				volTag)
		}
		_ = tw.Flush()
	}
	if len(findings) > shown {
		_, _ = fmt.Fprintf(w, "  %s(%d more jobs not shown)%s\n", dim, len(findings)-shown, reset)
	}
	_, _ = fmt.Fprintln(w)
}

func writeCostTable(w io.Writer, findings []analyze.Finding) {
	shown := min(5, len(findings))
	header := fmt.Sprintf("%d workflows", len(findings))
	if shown < len(findings) {
		header = fmt.Sprintf("top %d of %d", shown, len(findings))
	}
	_, _ = fmt.Fprintf(w, "%s── CI Cost (%s) ──%s\n", dim, header, reset)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.StripEscape)
	for _, f := range findings[:shown] {
		d, ok := f.Detail.(analyze.CostDetail)
		if !ok {
			continue
		}

		savings := ""
		if d.DailySavingsEstimate > 0 {
			savings = fmt.Sprintf("\t%ssave ~%.0f mins/day%s", esc(green), d.DailySavingsEstimate, esc(reset))
		}

		_, _ = fmt.Fprintf(tw, "  %s%s%s\t%s%.0f mins%s\t%s(%.0f/day)%s\t%s%d runs%s%s\n",
			esc(bold), d.Workflow, esc(reset),
			esc(cyan), d.BillableMinutes, esc(reset),
			esc(dim), d.DailyRate, esc(reset),
			esc(dim), d.TotalRuns, esc(reset),
			savings)

		// Show top 3 costliest jobs
		limit := min(3, len(d.Jobs))
		for i := range limit {
			j := d.Jobs[i]
			mult := ""
			if j.Multiplier > 1 {
				mult = fmt.Sprintf(" %s(%.0fx)%s", esc(yellow), j.Multiplier, esc(reset))
			}
			_, _ = fmt.Fprintf(tw, "  %s  %s%s\t%s%.0f mins%s%s\n",
				esc(dim), j.Name, esc(reset),
				esc(dim), j.BillableMinutes, esc(reset),
				mult)
		}
	}
	_ = tw.Flush()
	if len(findings) > shown {
		_, _ = fmt.Fprintf(w, "  %s(%d more workflows not shown)%s\n", dim, len(findings)-shown, reset)
	}
	_, _ = fmt.Fprintln(w)
}

func writeFailureTable(w io.Writer, findings []analyze.Finding) {
	// Sub-5% already filtered by postprocessor
	if len(findings) == 0 {
		return
	}

	shown := min(7, len(findings))
	_, _ = fmt.Fprintf(w, "%s── Failure Rates (%d) ──%s\n", dim, len(findings), reset)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range findings[:shown] {
		d, ok := f.Detail.(analyze.FailureDetail)
		if !ok {
			continue
		}

		rateColor := dim
		switch {
		case d.FailureRate >= 0.2:
			rateColor = red
		case d.FailureRate >= 0.05:
			rateColor = yellow
		}

		// Build breakdown string (sorted for stable output)
		conclusions := make([]string, 0, len(d.ByConclusion))
		for conclusion := range d.ByConclusion {
			conclusions = append(conclusions, conclusion)
		}
		slices.Sort(conclusions)
		var parts []string
		for _, conclusion := range conclusions {
			parts = append(parts, fmt.Sprintf("%s: %d", conclusion, d.ByConclusion[conclusion]))
		}
		if d.RetriedRuns > 0 {
			parts = append(parts, fmt.Sprintf("retried: %d (+%d attempts)", d.RetriedRuns, d.ExtraAttempts))
		}

		cancelNote := ""
		if d.CancellationCount > 0 {
			cancelNote = fmt.Sprintf("\t%s+%d cancelled (%.0f%%)%s", dim, d.CancellationCount, d.CancellationRate*100, reset)
		}

		failsAt := ""
		if len(d.FailingSteps) > 0 {
			top := d.FailingSteps[0]
			failsAt = fmt.Sprintf("\tfails at: %s%s%s", yellow, top.StepName, reset)
			if len(d.FailingSteps) > 1 {
				failsAt += fmt.Sprintf(" %s(+%d more)%s", dim, len(d.FailingSteps)-1, reset)
			}
		}

		_, _ = fmt.Fprintf(tw, "  %s%s%s\t%s%.0f%%%s\t%s(%d/%d runs)%s\t%s%s%s%s%s\n",
			bold, d.Workflow, reset,
			rateColor, d.FailureRate*100, reset,
			dim, d.FailureCount, d.TotalRuns, reset,
			dim, strings.Join(parts, ", "), reset,
			cancelNote, failsAt)
	}
	_ = tw.Flush()
	if len(findings) > shown {
		_, _ = fmt.Fprintf(w, "  %s(%d more not shown)%s\n", dim, len(findings)-shown, reset)
	}
	_, _ = fmt.Fprintln(w)
}

func writeOutlierTable(w io.Writer, findings []analyze.Finding) error {
	// Findings are already grouped by postprocessor into OutlierGroupDetail.
	// Sort by worst duration descending.
	sorted := make([]analyze.Finding, len(findings))
	copy(sorted, findings)
	slices.SortFunc(sorted, func(a, b analyze.Finding) int {
		ad, _ := a.Detail.(analyze.OutlierGroupDetail)
		bd, _ := b.Detail.(analyze.OutlierGroupDetail)
		if bd.WorstDuration > ad.WorstDuration {
			return 1
		}
		if bd.WorstDuration < ad.WorstDuration {
			return -1
		}
		return 0
	})

	_, _ = fmt.Fprintf(w, "%s── Outliers (%d groups) ──%s\n", dim, len(sorted), reset)

	maxSubject := 0
	for _, f := range sorted {
		d, _ := f.Detail.(analyze.OutlierGroupDetail)
		subject := d.WorkflowName
		if d.JobName != "" {
			subject += " / " + d.JobName
		}
		if len(subject) > maxSubject {
			maxSubject = len(subject)
		}
	}

	for _, f := range sorted {
		d, _ := f.Detail.(analyze.OutlierGroupDetail)
		subject := d.WorkflowName
		if d.JobName != "" {
			subject += " / " + d.JobName
		}
		durColor := yellow
		if d.MaxSeverity == analyze.SeverityCritical {
			durColor = red
		}
		countStr := fmt.Sprintf("%dx", d.Count)
		if d.Count == 1 {
			countStr = "  "
		}
		_, _ = fmt.Fprintf(w, "  %s %-*s  %s%-3s%s %s%-8s%s %sp%.0f%s  %s%s%s\n",
			severityDot(d.MaxSeverity), maxSubject, subject,
			bold, countStr, reset,
			durColor, fmtDur(d.WorstDuration), reset,
			dim, d.WorstPercentile, reset,
			dim, truncSHA(d.WorstCommitSHA), reset)
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

func writeChangePointTable(w io.Writer, findings []analyze.Finding, verbose bool) error {
	// Split by category (set by postprocessor)
	var actionable, oscillating, minor []analyze.Finding
	for _, f := range findings {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok {
			continue
		}
		switch d.Category {
		case analyze.CategoryOscillating:
			oscillating = append(oscillating, f)
		case analyze.CategoryMinor:
			minor = append(minor, f)
		default: // regression, speedup
			actionable = append(actionable, f)
		}
	}

	if len(actionable) > 0 {
		_, _ = fmt.Fprintf(w, "%s── Change Points (%d) ──%s\n", dim, len(actionable), reset)
		writeChangePointRows(w, actionable)
		_, _ = fmt.Fprintln(w)
	}

	if len(oscillating) > 0 {
		writeOscillatingJobs(w, oscillating)
	}

	switch {
	case verbose && len(minor) > 0:
		_, _ = fmt.Fprintf(w, "%s── Change Points (minor, %d) ──%s\n", dim, len(minor), reset)
		writeChangePointRows(w, minor)
		_, _ = fmt.Fprintln(w)
	case len(minor) > 0:
		_, _ = fmt.Fprintf(w, "  %s(%d minor change points hidden, use -v to show)%s\n\n", dim, len(minor), reset)
	}

	return nil
}

// writeOscillatingJobs summarizes jobs with 3+ change points — these are volatile, not changing.
func writeOscillatingJobs(w io.Writer, findings []analyze.Finding) {
	type jobSummary struct {
		name     string
		count    int
		current  analyze.Duration // after-mean of the latest change point
		earliest analyze.Duration // before-mean of the first change point
	}
	seen := make(map[string]bool)
	jobCounts := make(map[string]int)
	var summaries []jobSummary
	latest := make(map[string]analyze.ChangePointDetail)
	earliest := make(map[string]analyze.ChangePointDetail)

	for _, f := range findings {
		d, _ := f.Detail.(analyze.ChangePointDetail)
		jobCounts[d.JobName]++
		if !seen[d.JobName] {
			seen[d.JobName] = true
			summaries = append(summaries, jobSummary{name: d.JobName})
			earliest[d.JobName] = d
		}
		latest[d.JobName] = d
	}
	for i := range summaries {
		summaries[i].count = jobCounts[summaries[i].name]
		summaries[i].current = latest[summaries[i].name].AfterMean
		summaries[i].earliest = earliest[summaries[i].name].BeforeMean
	}

	_, _ = fmt.Fprintf(w, "%s── Oscillating Jobs (%d jobs, too volatile for reliable change detection) ──%s\n", dim, len(summaries), reset)
	for _, s := range summaries {
		icon := yellow + "~" + reset
		trend := dim + "stable" + reset
		if s.current > s.earliest+s.earliest/10 {
			trend = red + "trending up" + reset
		} else if s.current < s.earliest-s.earliest/10 {
			trend = green + "trending down" + reset
		}
		_, _ = fmt.Fprintf(w, "  %s %s  %s%d shifts%s, was %s now %s (%s)\n",
			icon, s.name, dim, s.count, reset,
			fmtDur(s.earliest), fmtDur(s.current), trend)
	}
	_, _ = fmt.Fprintln(w)
}

func writeChangePointRows(w io.Writer, findings []analyze.Finding) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.StripEscape)
	_, _ = fmt.Fprintf(tw, "  %sDIR\tJOB\tCHANGE\tBEFORE\tAFTER\tDATE\tCOMMIT\tP-VALUE\tSTATUS%s\n",
		dim, reset)
	for _, f := range findings {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok {
			continue
		}

		var icon, changeColor string
		if d.Direction == analyze.DirectionSpeedup {
			icon = esc(green) + "▼" + esc(reset)
			changeColor = green
		} else {
			icon = esc(red) + "▲" + esc(reset)
			changeColor = red
		}

		status := formatPersistence(d)

		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s%s%s\t%s\t%s\t%s\t%s%s%s\t%s\t%s\n",
			icon, d.JobName,
			esc(changeColor), fmt.Sprintf("%+.0f%%", d.PctChange), esc(reset),
			fmtDur(d.BeforeMean), fmtDur(d.AfterMean),
			d.Date.Format("2006-01-02"),
			esc(dim), truncSHA(d.CommitSHA), esc(reset),
			fmtPValueStr(d.PValue),
			status)
	}
	_ = tw.Flush()
}

// esc wraps an ANSI code in tabwriter escape markers so it's not counted for column width.
func esc(code string) string {
	return "\xff" + code + "\xff"
}

func fmtPValueStr(p float64) string {
	s := fmt.Sprintf("%.4f", p)
	var color string
	switch {
	case p < 0.01:
		color = green
	case p < 0.05:
		color = yellow
	default:
		color = dim
	}
	return esc(color) + s + esc(reset)
}

func formatPersistence(d analyze.ChangePointDetail) string {
	switch d.Persistence {
	case "persistent":
		return fmt.Sprintf("%s✓ %d runs%s", esc(green), d.PostChangeRuns, esc(reset))
	case "transient":
		return fmt.Sprintf("%stransient (%d runs)%s", esc(yellow), d.PostChangeRuns, esc(reset))
	case "inconclusive":
		return fmt.Sprintf("%s? %d runs%s", esc(dim), d.PostChangeRuns, esc(reset))
	default:
		return ""
	}
}

// severityDot returns a colored dot. Single visible char so alignment is consistent.
func severityDot(severity string) string {
	switch severity {
	case analyze.SeverityCritical:
		return red + "●" + reset
	case analyze.SeverityWarning:
		return yellow + "●" + reset
	default:
		return dim + "●" + reset
	}
}

func writeLegend(w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n%s", dim)
	_, _ = fmt.Fprint(w, "Volatility (p95/median): [variable] 1.3-2x  [spiky] 2-3x  [volatile] >3x\n")
	_, _ = fmt.Fprintf(w, "Outliers: %s●%s critical (p99+)  %s●%s warning (p95+)  %s●%s info\n",
		red, dim, yellow, dim, dim, dim)
	_, _ = fmt.Fprint(w, "Change points: ^ slowdown  v speedup | Status: N runs = persistent, transient = reverted, ? = too few runs\n")
	_, _ = fmt.Fprintf(w, "%s", reset)
}
