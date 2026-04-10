package output

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// TableFormatter outputs results as a human-readable table.
type TableFormatter struct{}

// Format implements Formatter.
func (TableFormatter) Format(w io.Writer, result analyze.AnalysisResult) error {
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
		if err := writeChangePointTable(w, changepoints); err != nil {
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

func writeSummaryTable(w io.Writer, findings []analyze.Finding) error {
	_, _ = fmt.Fprintln(w, "── Summary ──")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SUBJECT\tRUNS\tMEDIAN\tP95\tMIN\tMAX")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\n",
			d.Subject, d.TotalRuns,
			fmtDur(d.Median), fmtDur(d.P95), fmtDur(d.Min), fmtDur(d.Max))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

func writeOutlierTable(w io.Writer, findings []analyze.Finding) error {
	_, _ = fmt.Fprintf(w, "── Outliers (%d) ──\n", len(findings))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "SEVERITY\tSUBJECT\tDURATION\tPERCENTILE\tCOMMIT")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.OutlierDetail)
		if !ok {
			continue
		}
		subject := "workflow"
		if d.JobName != "" {
			subject = d.JobName
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\tp%.0f\t%s\n",
			severityIcon(f.Severity), subject,
			fmtDur(d.Duration), d.Percentile, truncSHA(d.CommitSHA))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

func writeChangePointTable(w io.Writer, findings []analyze.Finding) error {
	// Separate significant from noise
	var significant, other []analyze.Finding
	for _, f := range findings {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok {
			continue
		}
		if d.PValue < 0.05 {
			significant = append(significant, f)
		} else {
			other = append(other, f)
		}
	}

	if len(significant) > 0 {
		_, _ = fmt.Fprintf(w, "── Change Points (significant, p<0.05) (%d) ──\n", len(significant))
		writeChangePointRows(w, significant)
		_, _ = fmt.Fprintln(w)
	}

	if len(other) > 0 {
		_, _ = fmt.Fprintf(w, "── Change Points (insignificant) (%d) ──\n", len(other))
		writeChangePointRows(w, other)
		_, _ = fmt.Fprintln(w)
	}

	return nil
}

func writeChangePointRows(w io.Writer, findings []analyze.Finding) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "DIR\tJOB\tCHANGE\tBEFORE\tAFTER\tDATE\tCOMMIT\tP-VALUE")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok {
			continue
		}
		icon := "▲"
		if d.Direction == "speedup" {
			icon = "▼"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%+.0f%%\t%s\t%s\t%s\t%s\t%.4f\n",
			icon, d.JobName, d.PctChange,
			fmtDur(d.BeforeMean), fmtDur(d.AfterMean),
			findDate(f), truncSHA(d.CommitSHA), d.PValue)
	}
	_ = tw.Flush()
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

func severityIcon(severity string) string {
	switch severity {
	case "critical":
		return "!!!"
	case "warning":
		return "!!"
	default:
		return "!"
	}
}

func truncSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
