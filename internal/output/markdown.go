package output

import (
	"fmt"
	"io"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// MarkdownFormatter outputs results as a markdown report.
type MarkdownFormatter struct {
	Verbose bool
}

// Format implements Formatter.
func (MarkdownFormatter) Format(w io.Writer, result analyze.AnalysisResult) error {
	_, _ = fmt.Fprintf(w, "# CI Performance Report\n\n")
	_, _ = fmt.Fprintf(w, "**%d runs** analyzed (%s to %s)\n\n",
		result.Meta.TotalRuns,
		result.Meta.TimeRange[0].Format("2006-01-02"),
		result.Meta.TimeRange[1].Format("2006-01-02"))

	g := groupByType(result.Findings)

	for _, f := range g.Summaries {
		d, ok := f.Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "### %s\n", d.Workflow)
		_, _ = fmt.Fprintf(w, "%d runs, median %s, p95 %s, total CI time %s\n\n",
			d.Stats.TotalRuns, fmtDur(d.Stats.Median), fmtDur(d.Stats.P95), fmtTotalTime(d.Stats.TotalTime))

		if len(d.Jobs) > 0 {
			_, _ = fmt.Fprintln(w, "| Job | Runs | Median | P95 | Min | Max |")
			_, _ = fmt.Fprintln(w, "|-----|------|--------|-----|-----|-----|")
			for _, job := range d.Jobs {
				_, _ = fmt.Fprintf(w, "| %s | %d | %s | %s | %s | %s |\n",
					job.Name, job.Stats.TotalRuns,
					fmtDur(job.Stats.Median), fmtDur(job.Stats.P95),
					fmtDur(job.Stats.Min), fmtDur(job.Stats.Max))
			}
			_, _ = fmt.Fprintln(w)
		}
	}

	if len(g.Changepoints) > 0 {
		var notable []analyze.Finding
		for _, f := range g.Changepoints {
			if f.Severity != analyze.SeverityInfo {
				notable = append(notable, f)
			}
		}

		if len(notable) > 0 {
			_, _ = fmt.Fprintf(w, "## Performance Changes (%d)\n", len(notable))
			for _, f := range notable {
				d, ok := f.Detail.(analyze.ChangePointDetail)
				if !ok {
					continue
				}
				icon := analyze.DirectionSlowdown
				if d.Direction == analyze.DirectionSpeedup {
					icon = analyze.DirectionSpeedup
				}
				_, _ = fmt.Fprintf(w, "- **%s %+.0f%%** in `%s` at `%s` — %s -> %s (p=%.4f, %s, %d runs after)\n",
					icon, d.PctChange, d.JobName, truncSHA(d.CommitSHA),
					fmtDur(d.BeforeMean), fmtDur(d.AfterMean), d.PValue,
					d.Persistence, d.PostChangeRuns)
			}
			_, _ = fmt.Fprintln(w)
		}
	}

	if len(g.Outliers) > 0 {
		_, _ = fmt.Fprintf(w, "## Outliers (%d)\n", len(g.Outliers))
		_, _ = fmt.Fprintln(w, "| Severity | Subject | Duration | Percentile | Commit |")
		_, _ = fmt.Fprintln(w, "|----------|---------|----------|------------|--------|")
		for _, f := range g.Outliers {
			d, ok := f.Detail.(analyze.OutlierDetail)
			if !ok {
				continue
			}
			subject := d.WorkflowName
			if d.JobName != "" {
				subject += " / " + d.JobName
			}
			_, _ = fmt.Fprintf(w, "| %s | %s | %s | p%.0f | `%s` |\n",
				f.Severity, subject, fmtDur(d.Duration), d.Percentile, truncSHA(d.CommitSHA))
		}
		_, _ = fmt.Fprintln(w)
	}

	return nil
}
