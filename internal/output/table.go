package output

import (
	"fmt"
	"io"
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
	var summaries, outliers, changepoints []analyze.Finding
	for _, f := range result.Findings {
		switch f.Type {
		case "summary":
			summaries = append(summaries, f)
		case "outlier":
			outliers = append(outliers, f)
		case "changepoint":
			changepoints = append(changepoints, f)
		}
	}

	if len(summaries) > 0 {
		if err := writeSummaryTable(w, summaries); err != nil {
			return err
		}
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
	_, err := fmt.Fprintf(w, "\n%d runs analyzed (%s to %s)\n",
		result.Meta.TotalRuns,
		result.Meta.TimeRange[0].Format("2006-01-02"),
		result.Meta.TimeRange[1].Format("2006-01-02"))
	return err
}

// ANSI color codes
const (
	bold  = "\033[1m"
	dim   = "\033[2m"
	red   = "\033[31m"
	reset = "\033[0m"
)

func writeSummaryTable(w io.Writer, findings []analyze.Finding) error {
	// Findings are already sorted by total CI time descending from the analyzer
	for i, f := range findings {
		d, ok := f.Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}

		marker := ""
		if i == 0 {
			marker = red + " ← most CI time" + reset
		}

		if len(d.Jobs) <= 1 {
			// Single-job workflow: one compact line
			_, _ = fmt.Fprintf(w, "%s%s%s  %s%d runs, median %s, p95 %s, total %s%s%s\n",
				bold, d.Workflow, reset,
				dim, d.Stats.TotalRuns,
				fmtDur(d.Stats.Median), fmtDur(d.Stats.P95),
				fmtTotalTime(d.Stats.TotalTime), reset, marker)
		} else {
			// Multi-job workflow: header + tree
			_, _ = fmt.Fprintf(w, "%s%s%s  %s%d runs, median %s, p95 %s, total %s%s%s\n",
				bold, d.Workflow, reset,
				dim, d.Stats.TotalRuns,
				fmtDur(d.Stats.Median), fmtDur(d.Stats.P95),
				fmtTotalTime(d.Stats.TotalTime), reset, marker)

			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			for j, job := range d.Jobs {
				prefix := "├─"
				if j == len(d.Jobs)-1 {
					prefix = "└─"
				}
				highlight := ""
				if job.Stats.Median > d.Stats.Median/2 {
					highlight = bold
				}
				_, _ = fmt.Fprintf(tw, "  %s %s%s%s\t%d runs\tmedian %s\tp95 %s\tmin %s\tmax %s\n",
					prefix, highlight, job.Name, reset,
					job.Stats.TotalRuns,
					fmtDur(job.Stats.Median), fmtDur(job.Stats.P95),
					fmtDur(job.Stats.Min), fmtDur(job.Stats.Max))
			}
			_ = tw.Flush()
		}
		_, _ = fmt.Fprintln(w)
	}
	return nil
}

func fmtTotalTime(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func writeOutlierTable(w io.Writer, findings []analyze.Finding) error {
	_, _ = fmt.Fprintf(w, "── Outliers (%d) ──\n", len(findings))

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
		_, _ = fmt.Fprintf(w, "  %s %-*s  %-8s %-4s  %s\n",
			severityDot(r.severity), maxSubject, r.subject,
			r.duration, r.pct, r.commit)
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

func writeChangePointTable(w io.Writer, findings []analyze.Finding, verbose bool) error {
	var notable, minor []analyze.Finding
	for _, f := range findings {
		if f.Severity == "info" {
			minor = append(minor, f)
		} else {
			notable = append(notable, f)
		}
	}

	if len(notable) > 0 {
		_, _ = fmt.Fprintf(w, "── Change Points (%d) ──\n", len(notable))
		writeChangePointRows(w, notable)
		_, _ = fmt.Fprintln(w)
	}

	switch {
	case verbose && len(minor) > 0:
		_, _ = fmt.Fprintf(w, "── Change Points (minor, %d) ──\n", len(minor))
		writeChangePointRows(w, minor)
		_, _ = fmt.Fprintln(w)
	case len(minor) > 0 && len(notable) > 0:
		_, _ = fmt.Fprintf(w, "  (%d minor change points hidden, use -v to show)\n\n", len(minor))
	case len(minor) > 0:
		_, _ = fmt.Fprintf(w, "  (%d minor change points found, use -v to show)\n\n", len(minor))
	}

	return nil
}

func writeChangePointRows(w io.Writer, findings []analyze.Finding) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "DIR\tJOB\tCHANGE\tBEFORE\tAFTER\tDATE\tCOMMIT\tP-VALUE\tSTATUS")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok {
			continue
		}
		icon := red + "▲" + reset
		if d.Direction == "speedup" {
			icon = "\033[32m▼" + reset // green
		}
		status := formatPersistence(d)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%+.0f%%\t%s\t%s\t%s\t%s\t%.4f\t%s\n",
			icon, d.JobName, d.PctChange,
			fmtDur(d.BeforeMean), fmtDur(d.AfterMean),
			findDate(f), truncSHA(d.CommitSHA), d.PValue, status)
	}
	_ = tw.Flush()
}

func formatPersistence(d analyze.ChangePointDetail) string {
	switch d.Persistence {
	case "persistent":
		return fmt.Sprintf("✓ %d runs", d.PostChangeRuns)
	case "transient":
		return fmt.Sprintf("~ %d runs", d.PostChangeRuns)
	case "inconclusive":
		return fmt.Sprintf("? %d runs", d.PostChangeRuns)
	default:
		return ""
	}
}

func findDate(f analyze.Finding) string {
	// Extract date from description (format: "... at YYYY-MM-DD ...")
	desc := f.Description
	for i := range len(desc) - 10 {
		if desc[i] >= '2' && desc[i] <= '2' && desc[i+4] == '-' {
			return desc[i : i+10]
		}
	}
	return "?"
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

const (
	yellow = "\033[33m"
)

// severityDot returns a colored dot. Single visible char so tabwriter alignment is consistent.
func severityDot(severity string) string {
	switch severity {
	case "critical":
		return red + "●" + reset
	case "warning":
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
