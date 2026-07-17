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
func (m MarkdownFormatter) Format(w io.Writer, result *analyze.AnalysisResult) error {
	_, _ = fmt.Fprintf(w, "# CI Performance Report\n\n")
	_, _ = fmt.Fprintf(w, "**%d runs** analyzed (%s to %s)\n\n",
		result.Meta.TotalRuns,
		result.Meta.TimeRange[0].Format("2006-01-02"),
		result.Meta.TimeRange[1].Format("2006-01-02"))

	g := groupByType(result.Findings)

	mdWriteSummaries(w, g.Summaries)
	mdWriteChangepoints(w, g.Changepoints, m.Verbose)
	mdWriteOutliers(w, g.Outliers)
	mdWriteFailures(w, g.Failures)
	mdWriteCost(w, g.Costs)
	mdWritePipelines(w, g.Pipelines)
	mdWriteRunners(w, g.Runners)
	mdWriteSteps(w, g.Steps)

	return nil
}

func mdWriteSummaries(w io.Writer, findings []analyze.Finding) {
	for _, f := range findings {
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
					escMD(job.Name), job.Stats.TotalRuns,
					fmtDur(job.Stats.Median), fmtDur(job.Stats.P95),
					fmtDur(job.Stats.Min), fmtDur(job.Stats.Max))
			}
			_, _ = fmt.Fprintln(w)
		}
	}
}

// mdWriteChangepoints mirrors the table format's verbosity contract: minor
// (info-severity) change points are hidden behind -v but counted, so a quiet
// report still says what it is not showing.
func mdWriteChangepoints(w io.Writer, findings []analyze.Finding, verbose bool) {
	var notable, minor []analyze.Finding
	for _, f := range findings {
		if f.Severity != analyze.SeverityInfo {
			notable = append(notable, f)
		} else {
			minor = append(minor, f)
		}
	}
	if verbose {
		notable = append(notable, minor...)
		minor = nil
	}

	if len(notable) > 0 {
		_, _ = fmt.Fprintf(w, "## Performance Changes (%d)\n", len(notable))
		for _, f := range notable {
			d, ok := f.Detail.(analyze.ChangePointDetail)
			if !ok {
				continue
			}
			icon := "▲"
			if d.Direction == analyze.DirectionSpeedup {
				icon = "▼"
			}
			_, _ = fmt.Fprintf(w, "- **%s %+.0f%%** in `%s` at `%s` — %s -> %s (p=%.4f, %s, %d runs after)\n",
				icon, d.PctChange, d.JobName, truncSHA(d.CommitSHA),
				fmtDur(d.BeforeMean), fmtDur(d.AfterMean), d.PValue,
				d.Persistence, d.PostChangeRuns)
		}
		_, _ = fmt.Fprintln(w)
	}
	if len(minor) > 0 {
		plural := "s"
		if len(minor) == 1 {
			plural = ""
		}
		_, _ = fmt.Fprintf(w, "_%d minor change point%s hidden — use -v to include them._\n\n", len(minor), plural)
	}
}

func mdWriteOutliers(w io.Writer, findings []analyze.Finding) {
	if len(findings) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "## Outliers (%d groups)\n", len(findings))
	_, _ = fmt.Fprintln(w, "| Severity | Subject | Count | Worst Duration | Percentile | Commit |")
	_, _ = fmt.Fprintln(w, "|----------|---------|-------|----------------|------------|--------|")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.OutlierGroupDetail)
		if !ok {
			continue
		}
		subject := escMD(d.WorkflowName)
		if d.JobName != "" {
			subject += " / " + escMD(d.JobName)
		}
		_, _ = fmt.Fprintf(w, "| %s | %s | %d | %s | %s | `%s` |\n",
			d.MaxSeverity, subject, d.Count, fmtDur(d.WorstDuration), fmtPercentile(d.WorstPercentile), truncSHA(d.WorstCommitSHA))
	}
	_, _ = fmt.Fprintln(w)
}

func mdWriteFailures(w io.Writer, findings []analyze.Finding) {
	if len(findings) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "## Failure Rates (%d)\n", len(findings))
	_, _ = fmt.Fprintln(w, "| Workflow | Rate | Failures | Kind | Trend |")
	_, _ = fmt.Fprintln(w, "|----------|------|----------|------|-------|")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.FailureDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "| %s | %.0f%% | %d/%d | %s | %s |\n",
			escMD(d.Workflow), d.FailureRate*100, d.FailureCount, d.TotalRuns, d.FailureKind, d.Trend)
	}
	_, _ = fmt.Fprintln(w)
}

func mdWriteCost(w io.Writer, findings []analyze.Finding) {
	if len(findings) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "## Cost (%d workflows)\n", len(findings))
	_, _ = fmt.Fprintln(w, "| Workflow | Billable mins | Mins/day | Runs |")
	_, _ = fmt.Fprintln(w, "|----------|---------------|----------|------|")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.CostDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "| %s | %.0f | %.0f | %d |\n",
			escMD(d.Workflow), d.BillableMinutes, d.DailyRate, d.TotalRuns)
	}
	_, _ = fmt.Fprintln(w)
}

func mdWritePipelines(w io.Writer, findings []analyze.Finding) {
	if len(findings) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "## Pipeline Structure")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.PipelineDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "**%s** — %.0f%% parallel, wall-clock %s, total job time %s\n\n",
			escMD(d.Workflow), d.Parallelism*100, fmtDur(d.MedianWallClock), fmtDur(d.MedianJobSum))
		for _, st := range d.Stages {
			marker := ""
			if st.Name == d.CriticalPath {
				marker = " ← critical path"
			}
			_, _ = fmt.Fprintf(w, "- %s: %s (%.0f%%)%s\n",
				escMD(st.Name), fmtDur(st.Duration), st.PctOfPipeline, marker)
		}
		_, _ = fmt.Fprintln(w)
	}
}

func mdWriteRunners(w io.Writer, findings []analyze.Finding) {
	if len(findings) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "## Runner Sizing")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.RunnerDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "- **%s / %s** (%d cores): %s\n",
			escMD(d.WorkflowName), escMD(d.JobName), d.Cores, d.Suggestion)
	}
	_, _ = fmt.Fprintln(w)
}

func mdWriteSteps(w io.Writer, findings []analyze.Finding) {
	if len(findings) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "## Step-Level Timing")
	for _, f := range findings {
		d, ok := f.Detail.(analyze.StepTimingDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "### %s / %s\n", escMD(d.WorkflowName), escMD(d.JobName))
		_, _ = fmt.Fprintln(w, "| Step | Median | % of job | Volatility |")
		_, _ = fmt.Fprintln(w, "|------|--------|----------|------------|")
		for _, st := range d.Steps {
			_, _ = fmt.Fprintf(w, "| %s | %s | %.0f%% | %.1fx |\n",
				escMD(st.Name), fmtDur(st.Median), st.PctOfJob, st.Volatility)
		}
		_, _ = fmt.Fprintln(w)
	}
}
