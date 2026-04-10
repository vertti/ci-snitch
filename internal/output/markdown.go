package output

import (
	"fmt"
	"io"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// MarkdownFormatter outputs results as a markdown report.
type MarkdownFormatter struct{}

// Format implements Formatter.
func (MarkdownFormatter) Format(w io.Writer, result analyze.AnalysisResult) error {
	_, _ = fmt.Fprintf(w, "# CI Performance Report\n\n")
	_, _ = fmt.Fprintf(w, "**%d runs** analyzed (%s to %s)\n\n",
		result.Meta.TotalRuns,
		result.Meta.TimeRange[0].Format("2006-01-02"),
		result.Meta.TimeRange[1].Format("2006-01-02"))

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
		_, _ = fmt.Fprintln(w, "## Summary")
		_, _ = fmt.Fprintln(w, "| Subject | Runs | Median | P95 | Min | Max |")
		_, _ = fmt.Fprintln(w, "|---------|------|--------|-----|-----|-----|")
		for _, f := range summaries {
			d, ok := f.Detail.(analyze.SummaryDetail)
			if !ok {
				continue
			}
			_, _ = fmt.Fprintf(w, "| %s | %d | %s | %s | %s | %s |\n",
				d.Subject, d.TotalRuns,
				fmtDur(d.Median), fmtDur(d.P95), fmtDur(d.Min), fmtDur(d.Max))
		}
		_, _ = fmt.Fprintln(w)
	}

	if len(changepoints) > 0 {
		var significant []analyze.Finding
		for _, f := range changepoints {
			d, ok := f.Detail.(analyze.ChangePointDetail)
			if ok && d.PValue < 0.05 {
				significant = append(significant, f)
			}
		}

		if len(significant) > 0 {
			_, _ = fmt.Fprintf(w, "## Significant Performance Changes (%d)\n", len(significant))
			for _, f := range significant {
				d, ok := f.Detail.(analyze.ChangePointDetail)
				if !ok {
					continue
				}
				icon := "slowdown"
				if d.Direction == "speedup" {
					icon = "speedup"
				}
				_, _ = fmt.Fprintf(w, "- **%s %+.0f%%** in `%s` at `%s` — %s -> %s (p=%.4f)\n",
					icon, d.PctChange, d.JobName, truncSHA(d.CommitSHA),
					fmtDur(d.BeforeMean), fmtDur(d.AfterMean), d.PValue)
			}
			_, _ = fmt.Fprintln(w)
		}
	}

	if len(outliers) > 0 {
		_, _ = fmt.Fprintf(w, "## Outliers (%d)\n", len(outliers))
		_, _ = fmt.Fprintln(w, "| Severity | Subject | Duration | Percentile | Commit |")
		_, _ = fmt.Fprintln(w, "|----------|---------|----------|------------|--------|")
		for _, f := range outliers {
			d, ok := f.Detail.(analyze.OutlierDetail)
			if !ok {
				continue
			}
			subject := "workflow"
			if d.JobName != "" {
				subject = d.JobName
			}
			_, _ = fmt.Fprintf(w, "| %s | %s | %s | p%.0f | `%s` |\n",
				f.Severity, subject, fmtDur(d.Duration), d.Percentile, truncSHA(d.CommitSHA))
		}
		_, _ = fmt.Fprintln(w)
	}

	return nil
}
