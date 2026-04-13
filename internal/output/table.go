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

	// Group findings by type
	var summaries, outliers, changepoints, failures []analyze.Finding
	for _, f := range result.Findings {
		switch f.Type {
		case "summary":
			summaries = append(summaries, f)
		case "outlier":
			outliers = append(outliers, f)
		case "changepoint":
			changepoints = append(changepoints, f)
		case "failure":
			failures = append(failures, f)
		}
	}

	if len(summaries) > 0 {
		writeTriageHeader(w, summaries, changepoints, failures)
		if err := writeSummaryTable(w, summaries); err != nil {
			return err
		}
	}

	if len(failures) > 0 {
		writeFailureTable(w, failures)
	}

	if len(outliers) > 0 {
		if err := writeOutlierTable(w, outliers); err != nil {
			return err
		}
	}

	if len(changepoints) > 0 {
		if err := writeChangePointTable(w, changepoints, t.Verbose); err != nil {
			return err
		}
	}

	// Meta
	_, _ = fmt.Fprintf(w, "\n%s%d runs analyzed%s (%s to %s)\n",
		dim, result.Meta.TotalRuns, reset,
		result.Meta.TimeRange[0].Format("2006-01-02"),
		result.Meta.TimeRange[1].Format("2006-01-02"))

	// Legend: only show entries for sections that appeared
	writeLegend(w, len(summaries) > 0, len(outliers) > 0, len(changepoints) > 0)
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

	// Active regressions (persistent critical/warning change points)
	var regressions []string
	for _, f := range changepoints {
		if f.Severity == analyze.SeverityInfo {
			continue
		}
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok || d.Direction != analyze.DirectionSlowdown || d.Persistence == "transient" {
			continue
		}
		regressions = append(regressions, fmt.Sprintf("%s %+.0f%%", d.JobName, d.PctChange))
	}
	if len(regressions) > 0 {
		_, _ = fmt.Fprintf(w, "  %sRegressions:%s  ", dim, reset)
		for i, r := range regressions {
			if i > 0 {
				_, _ = fmt.Fprint(w, ", ")
			}
			_, _ = fmt.Fprintf(w, "%s%s%s", red, r, reset)
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
		if len(d.Jobs) > 1 {
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

		_, _ = fmt.Fprintf(w, "%s%s%s  %d runs, median %s%s%s, p95 %s%s%s, total %s%s%s%s%s\n",
			bold, d.Workflow, reset,
			d.Stats.TotalRuns,
			cyan, fmtDur(d.Stats.Median), reset,
			cyan, fmtDur(d.Stats.P95), reset,
			bold, fmtTotalTime(d.Stats.TotalTime), reset,
			volTag, marker)

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

			_, _ = fmt.Fprintf(tw, "%s%s%s\t%d runs\tmedian %s%s%s\tp95 %s%s%s\ttotal %s%s%s%s%s\n",
				bold, d.Workflow, reset,
				d.Stats.TotalRuns,
				cyan, fmtDur(d.Stats.Median), reset,
				cyan, fmtDur(d.Stats.P95), reset,
				bold, fmtTotalTime(d.Stats.TotalTime), reset,
				volTag, marker)
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

func fmtTotalTime(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func writeFailureTable(w io.Writer, findings []analyze.Finding) {
	_, _ = fmt.Fprintf(w, "%s── Failure Rates (%d) ──%s\n", dim, len(findings), reset)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range findings {
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

		_, _ = fmt.Fprintf(tw, "  %s%s%s\t%s%.0f%%%s\t%s(%d/%d runs)%s\t%s%s%s\n",
			bold, d.Workflow, reset,
			rateColor, d.FailureRate*100, reset,
			dim, d.FailureCount, d.TotalRuns, reset,
			dim, strings.Join(parts, ", "), reset)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
}

func writeOutlierTable(w io.Writer, findings []analyze.Finding) error {
	_, _ = fmt.Fprintf(w, "%s── Outliers (%d) ──%s\n", dim, len(findings), reset)

	// Build rows without ANSI so we can measure column widths
	type outlierRow struct {
		severity string
		subject  string
		duration string
		pct      string
		commit   string
	}
	rows := make([]outlierRow, 0, len(findings))
	maxSubject := 0
	for _, f := range findings {
		d, ok := f.Detail.(analyze.OutlierDetail)
		if !ok {
			continue
		}
		subject := d.WorkflowName
		if d.JobName != "" {
			subject += " / " + d.JobName
		}
		if len(subject) > maxSubject {
			maxSubject = len(subject)
		}
		rows = append(rows, outlierRow{
			severity: f.Severity,
			subject:  subject,
			duration: fmtDur(d.Duration),
			pct:      fmt.Sprintf("p%.0f", d.Percentile),
			commit:   truncSHA(d.CommitSHA),
		})
	}

	for _, r := range rows {
		durColor := yellow
		if r.severity == analyze.SeverityCritical {
			durColor = red
		}
		_, _ = fmt.Fprintf(w, "  %s %-*s  %s%-8s%s %s%-4s%s  %s%s%s\n",
			severityDot(r.severity), maxSubject, r.subject,
			durColor, r.duration, reset,
			dim, r.pct, reset,
			dim, r.commit, reset)
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

func writeChangePointTable(w io.Writer, findings []analyze.Finding, verbose bool) error {
	var notable, minor []analyze.Finding
	for _, f := range findings {
		if f.Severity == analyze.SeverityInfo {
			minor = append(minor, f)
		} else {
			notable = append(notable, f)
		}
	}

	if len(notable) > 0 {
		_, _ = fmt.Fprintf(w, "%s── Change Points (%d) ──%s\n", dim, len(notable), reset)
		writeChangePointRows(w, notable)
		_, _ = fmt.Fprintln(w)
	}

	switch {
	case verbose && len(minor) > 0:
		_, _ = fmt.Fprintf(w, "%s── Change Points (minor, %d) ──%s\n", dim, len(minor), reset)
		writeChangePointRows(w, minor)
		_, _ = fmt.Fprintln(w)
	case len(minor) > 0 && len(notable) > 0:
		_, _ = fmt.Fprintf(w, "  %s(%d minor change points hidden, use -v to show)%s\n\n", dim, len(minor), reset)
	case len(minor) > 0:
		_, _ = fmt.Fprintf(w, "  %s(%d minor change points found, use -v to show)%s\n\n", dim, len(minor), reset)
	}

	return nil
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

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
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

func truncSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func writeLegend(w io.Writer, _, _, _ bool) {
	_, _ = fmt.Fprintf(w, "\n%s", dim)
	_, _ = fmt.Fprint(w, "Volatility (p95/median): [variable] 1.3-2x  [spiky] 2-3x  [volatile] >3x\n")
	_, _ = fmt.Fprintf(w, "Outliers: %s●%s critical (p99+)  %s●%s warning (p95+)  %s●%s info\n",
		red, dim, yellow, dim, dim, dim)
	_, _ = fmt.Fprint(w, "Change points: ^ slowdown  v speedup | Status: N runs = persistent, transient = reverted, ? = too few runs\n")
	_, _ = fmt.Fprintf(w, "%s", reset)
}
