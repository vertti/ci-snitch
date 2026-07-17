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
	// pal overrides palette derivation from the writer. Nil (the common
	// case) means derive per Format call: colored on a TTY, plain otherwise.
	pal *palette
}

// Format implements Formatter.
func (t TableFormatter) Format(w io.Writer, result *analyze.AnalysisResult) error {
	p := paletteFor(w)
	if t.pal != nil {
		p = *t.pal
	}

	if len(result.Findings) == 0 {
		_, err := fmt.Fprintln(w, "No findings.")
		return err
	}

	g := groupByType(result.Findings)

	if len(g.Summaries) > 0 {
		p.writeTriageHeader(w, g.Summaries, g.Changepoints, g.Failures)
		p.writeSummaryTable(w, g.Summaries)
	}

	if len(g.Steps) > 0 {
		p.writeStepTable(w, g.Steps, t.Verbose)
	}

	if len(g.Pipelines) > 0 {
		p.writePipelineTable(w, g.Pipelines)
	}

	if len(g.Runners) > 0 {
		p.writeRunnerTable(w, g.Runners)
	}

	if len(g.Costs) > 0 {
		p.writeCostTable(w, g.Costs)
	}

	if len(g.Failures) > 0 {
		p.writeFailureTable(w, g.Failures)
	}

	if len(g.Outliers) > 0 {
		p.writeOutlierTable(w, g.Outliers)
	}

	if len(g.Changepoints) > 0 {
		p.writeChangePointTable(w, g.Changepoints, t.Verbose)
	}

	// Meta
	_, _ = fmt.Fprintf(w, "\n%s%d runs analyzed%s (%s to %s)\n",
		p.dim, result.Meta.TotalRuns, p.reset,
		result.Meta.TimeRange[0].Format("2006-01-02"),
		result.Meta.TimeRange[1].Format("2006-01-02"))

	// Legend
	p.writeLegend(w)
	return nil
}

func (p *palette) writeTriageHeader(w io.Writer, summaries, changepoints, failures []analyze.Finding) {
	_, _ = fmt.Fprintf(w, "%s── Triage ──%s\n", p.dim, p.reset)
	p.writeTriageTopCITime(w, summaries)
	p.writeTriageVolatile(w, summaries)
	p.writeTriageRegressions(w, changepoints)
	p.writeTriageFlaky(w, failures)
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeTriageTopCITime(w io.Writer, summaries []analyze.Finding) {
	_, _ = fmt.Fprintf(w, "  %sTop CI time:%s  ", p.dim, p.reset)
	count := min(3, len(summaries))
	for i := range count {
		d, ok := summaries[i].Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		if i > 0 {
			_, _ = fmt.Fprint(w, "  ")
		}
		_, _ = fmt.Fprintf(w, "%s%s%s %s(%s)%s", p.bold, d.Workflow, p.reset, p.dim, fmtTotalTime(d.Stats.TotalTime), p.reset)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeTriageVolatile(w io.Writer, summaries []analyze.Finding) {
	var volatile []string
	for _, f := range summaries {
		d, ok := f.Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		if d.Stats.VolatilityLabel == analyze.VolatilityVolatile || d.Stats.VolatilityLabel == analyze.VolatilitySpiky {
			volatile = append(volatile, d.Workflow)
		}
	}
	if len(volatile) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "  %sUnpredictable:%s  ", p.dim, p.reset)
	for i, name := range volatile {
		if i > 0 {
			_, _ = fmt.Fprint(w, ", ")
		}
		_, _ = fmt.Fprintf(w, "%s%s%s", p.yellow, name, p.reset)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeTriageRegressions(w io.Writer, changepoints []analyze.Finding) {
	var regressions []analyze.ChangePointDetail
	for _, f := range changepoints {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if ok && isRegressionSlowdown(&d) {
			regressions = append(regressions, d)
		}
	}
	if len(regressions) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "  %sRegressions:%s  ", p.dim, p.reset)
	shown := min(5, len(regressions))
	for i := range regressions[:shown] {
		if i > 0 {
			_, _ = fmt.Fprint(w, ", ")
		}
		_, _ = fmt.Fprintf(w, "%s%s %+.0f%%%s", p.red, regressions[i].JobName, regressions[i].PctChange, p.reset)
	}
	if len(regressions) > shown {
		_, _ = fmt.Fprintf(w, "%s, +%d more%s", p.dim, len(regressions)-shown, p.reset)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeTriageFlaky(w io.Writer, failures []analyze.Finding) {
	if len(failures) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "  %sFlaky:%s  ", p.dim, p.reset)
	count := min(3, len(failures))
	for i := range count {
		d, ok := failures[i].Detail.(analyze.FailureDetail)
		if !ok {
			continue
		}
		if i > 0 {
			_, _ = fmt.Fprint(w, ", ")
		}
		_, _ = fmt.Fprintf(w, "%s%s%s %s(%.0f%%)%s", p.red, d.Workflow, p.reset, p.dim, d.FailureRate*100, p.reset)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeSummaryTable(w io.Writer, findings []analyze.Finding) {
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
		marker := p.mostCITimeMarker(mf.idx, firstIdx, len(findings))
		volTag := p.fmtVolatility(d.Stats.VolatilityLabel)
		queueTag := p.fmtQueueTime(d.Queue)

		_, _ = fmt.Fprintf(w, "%s%s%s  %d runs, median %s%s%s, p95 %s%s%s, total %s%s%s%s%s%s\n",
			p.bold, d.Workflow, p.reset,
			d.Stats.TotalRuns,
			p.cyan, fmtDur(d.Stats.Median), p.reset,
			p.cyan, fmtDur(d.Stats.P95), p.reset,
			p.bold, fmtTotalTime(d.Stats.TotalTime), p.reset,
			volTag, queueTag, marker)

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for j, job := range d.Jobs {
			prefix := "  |-"
			if j == len(d.Jobs)-1 {
				prefix = "  `-"
			}
			jobVol := p.fmtVolatility(job.Stats.VolatilityLabel)
			_, _ = fmt.Fprintf(tw, "%s%s%s %s\t%d runs\tmedian %s\tp95 %s\tmin %s\tmax %s%s\n",
				p.dim, prefix, p.reset, job.Name,
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
			marker := p.mostCITimeMarker(sf.idx, firstIdx, len(findings))
			volTag := p.fmtVolatility(d.Stats.VolatilityLabel)
			queueTag := p.fmtQueueTime(d.Queue)

			_, _ = fmt.Fprintf(tw, "%s%s%s\t%d runs\tmedian %s%s%s\tp95 %s%s%s\ttotal %s%s%s%s%s%s\n",
				p.bold, d.Workflow, p.reset,
				d.Stats.TotalRuns,
				p.cyan, fmtDur(d.Stats.Median), p.reset,
				p.cyan, fmtDur(d.Stats.P95), p.reset,
				p.bold, fmtTotalTime(d.Stats.TotalTime), p.reset,
				volTag, queueTag, marker)
		}
		_ = tw.Flush()
		_, _ = fmt.Fprintln(w)
	}

}

type indexedFinding struct {
	idx     int
	finding analyze.Finding
}

func (p *palette) mostCITimeMarker(idx, firstIdx, total int) string {
	if idx == firstIdx && total > 1 {
		return p.red + " << most CI time" + p.reset
	}
	return ""
}

func (p *palette) fmtVolatility(label string) string {
	switch label {
	case analyze.VolatilityVolatile:
		return " " + p.red + "[volatile]" + p.reset
	case analyze.VolatilitySpiky:
		return " " + p.yellow + "[spiky]" + p.reset
	case analyze.VolatilityVariable:
		return " " + p.dim + "[variable]" + p.reset
	default:
		return ""
	}
}

func (p *palette) fmtQueueTime(q analyze.QueueStats) string {
	// Only show queue time when median is notable (> 5 seconds)
	if q.Median.Std() <= 5*time.Second {
		return ""
	}
	return fmt.Sprintf(" %s[queue %s]%s", p.yellow, fmtDur(q.Median), p.reset)
}

func (p *palette) writeStepTable(w io.Writer, findings []analyze.Finding, verbose bool) {
	shown := len(findings)
	if !verbose {
		shown = min(5, shown)
	}
	_, _ = fmt.Fprintf(w, "%s── Step Breakdown (top %d jobs) ──%s\n", p.dim, shown, p.reset)

	for _, f := range findings[:shown] {
		d, ok := f.Detail.(analyze.StepTimingDetail)
		if !ok {
			continue
		}

		_, _ = fmt.Fprintf(w, "  %s%s%s %s/ %s%s\n",
			p.bold, d.WorkflowName, p.reset, p.dim, d.JobName, p.reset)

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, st := range d.Steps {
			volTag := ""
			if st.Volatility >= 2.0 {
				volTag = fmt.Sprintf(" %s[%.1fx]%s", p.yellow, st.Volatility, p.reset)
			}
			_, _ = fmt.Fprintf(tw, "    %s\tmedian %s\tp95 %s\t%s%.0f%% of job%s%s\n",
				st.Name,
				fmtDur(st.Median), fmtDur(st.P95),
				p.dim, st.PctOfJob, p.reset,
				volTag)
		}
		_ = tw.Flush()
	}
	if len(findings) > shown {
		_, _ = fmt.Fprintf(w, "  %s(%d more jobs hidden, use -v to show)%s\n", p.dim, len(findings)-shown, p.reset)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writePipelineTable(w io.Writer, findings []analyze.Finding) {
	_, _ = fmt.Fprintf(w, "%s── Pipeline Structure ──%s\n", p.dim, p.reset)

	for _, f := range findings {
		d, ok := f.Detail.(analyze.PipelineDetail)
		if !ok {
			continue
		}

		_, _ = fmt.Fprintf(w, "  %s%s%s  %s%.0f%% parallel%s, wall-clock %s%s%s, total job time %s%s%s\n",
			p.bold, d.Workflow, p.reset,
			p.cyan, d.Parallelism*100, p.reset,
			p.cyan, fmtDur(d.MedianWallClock), p.reset,
			p.dim, fmtDur(d.MedianJobSum), p.reset)

		for i, stage := range d.Stages {
			prefix := "  |-"
			if i == len(d.Stages)-1 {
				prefix = "  `-"
			}
			arrow := ""
			if stage.Sequential {
				arrow = " " + p.yellow + "<< waits" + p.reset
				if stage.PotentialSavings.Std() >= time.Minute {
					arrow += " " + p.dim + "(~" + fmtDur(stage.PotentialSavings) + "/run if parallel)" + p.reset
				}
			}
			critical := ""
			if stage.Name == d.CriticalPath {
				critical = " " + p.red + "<< critical path" + p.reset
			}
			_, _ = fmt.Fprintf(w, "  %s%s%s %s  %s  %.0f%%%s%s\n",
				p.dim, prefix, p.reset,
				stage.Name, fmtDur(stage.Duration), stage.PctOfPipeline,
				arrow, critical)
		}
		_, _ = fmt.Fprintln(w)
	}
}

func (p *palette) writeRunnerTable(w io.Writer, findings []analyze.Finding) {
	_, _ = fmt.Fprintf(w, "%s── Runner Sizing ──%s\n", p.dim, p.reset)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range findings {
		d, ok := f.Detail.(analyze.RunnerDetail)
		if !ok {
			continue
		}
		icon := p.yellow + "▼" + p.reset // oversized: downsize
		if d.Issue == analyze.IssueUndersized {
			icon = p.cyan + "▲" + p.reset // undersized: upsize
		}
		_, _ = fmt.Fprintf(tw, "  %s %s / %s\t%s%d cores%s\tmedian %s\t%s\n",
			icon, d.WorkflowName, d.JobName,
			p.bold, d.Cores, p.reset,
			fmtDur(d.MedianDur),
			d.Suggestion)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeCostTable(w io.Writer, findings []analyze.Finding) {
	shown := min(5, len(findings))
	header := fmt.Sprintf("%d workflows", len(findings))
	if shown < len(findings) {
		header = fmt.Sprintf("top %d of %d", shown, len(findings))
	}
	_, _ = fmt.Fprintf(w, "%s── CI Cost (%s) ──%s\n", p.dim, header, p.reset)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.StripEscape)
	for _, f := range findings[:shown] {
		d, ok := f.Detail.(analyze.CostDetail)
		if !ok {
			continue
		}

		_, _ = fmt.Fprintf(tw, "  %s%s%s\t%s%.0f mins%s\t%s(%.0f/day)%s\t%s%d runs%s\n",
			esc(p.bold), d.Workflow, esc(p.reset),
			esc(p.cyan), d.BillableMinutes, esc(p.reset),
			esc(p.dim), d.DailyRate, esc(p.reset),
			esc(p.dim), d.TotalRuns, esc(p.reset))

		// Show top 3 costliest jobs
		limit := min(3, len(d.Jobs))
		for i := range limit {
			j := d.Jobs[i]
			mult := ""
			if j.Multiplier > 1 {
				mult = fmt.Sprintf(" %s(%.0fx)%s", esc(p.yellow), j.Multiplier, esc(p.reset))
			}
			_, _ = fmt.Fprintf(tw, "  %s  %s%s\t%s%.0f mins%s%s\n",
				esc(p.dim), j.Name, esc(p.reset),
				esc(p.dim), j.BillableMinutes, esc(p.reset),
				mult)
		}
	}
	_ = tw.Flush()
	if len(findings) > shown {
		_, _ = fmt.Fprintf(w, "  %s(%d more workflows not shown)%s\n", p.dim, len(findings)-shown, p.reset)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeFailureTable(w io.Writer, findings []analyze.Finding) {
	// Sub-5% already filtered by postprocessor
	if len(findings) == 0 {
		return
	}

	shown := min(7, len(findings))
	_, _ = fmt.Fprintf(w, "%s── Failure Rates (%d) ──%s\n", p.dim, len(findings), p.reset)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range findings[:shown] {
		d, ok := f.Detail.(analyze.FailureDetail)
		if !ok {
			continue
		}

		rateColor := p.dim
		switch {
		case d.FailureRate >= 0.2:
			rateColor = p.red
		case d.FailureRate >= 0.05:
			rateColor = p.yellow
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

		failsAt := ""
		if len(d.FailingSteps) > 0 {
			if top, dominant := dominantFailingStep(&d); dominant {
				failsAt = fmt.Sprintf("\tfails at: %s%s%s", p.yellow, top.StepName, p.reset)
			} else {
				failsAt = fmt.Sprintf("\tfailures across %d steps %s(top: %s)%s",
					len(d.FailingSteps), p.dim, top.StepName, p.reset)
			}
		}

		_, _ = fmt.Fprintf(tw, "  %s%s%s\t%s%.0f%%%s\t%s(%d/%d runs)%s\t%s%s%s%s\n",
			p.bold, d.Workflow, p.reset,
			rateColor, d.FailureRate*100, p.reset,
			p.dim, d.FailureCount, d.TotalRuns, p.reset,
			p.dim, strings.Join(parts, ", "), p.reset,
			failsAt)
	}
	_ = tw.Flush()
	if len(findings) > shown {
		_, _ = fmt.Fprintf(w, "  %s(%d more not shown)%s\n", p.dim, len(findings)-shown, p.reset)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeOutlierTable(w io.Writer, findings []analyze.Finding) {
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

	_, _ = fmt.Fprintf(w, "%s── Outliers (%d groups) ──%s\n", p.dim, len(sorted), p.reset)

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
		durColor := p.yellow
		if d.MaxSeverity == analyze.SeverityCritical {
			durColor = p.red
		}
		countStr := fmt.Sprintf("%dx", d.Count)
		if d.Count == 1 {
			countStr = "  "
		}
		_, _ = fmt.Fprintf(w, "  %s %-*s  %s%-3s%s %s%-8s%s %s%s%s  %s%s%s\n",
			p.severityDot(d.MaxSeverity), maxSubject, subject,
			p.bold, countStr, p.reset,
			durColor, fmtDur(d.WorstDuration), p.reset,
			p.dim, fmtPercentile(d.WorstPercentile), p.reset,
			p.dim, truncSHA(d.WorstCommitSHA), p.reset)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeChangePointTable(w io.Writer, findings []analyze.Finding, verbose bool) {
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
		_, _ = fmt.Fprintf(w, "%s── Change Points (%d) ──%s\n", p.dim, len(actionable), p.reset)
		p.writeChangePointRows(w, actionable)
		_, _ = fmt.Fprintln(w)
	}

	if len(oscillating) > 0 {
		p.writeOscillatingJobs(w, oscillating)
	}

	switch {
	case verbose && len(minor) > 0:
		_, _ = fmt.Fprintf(w, "%s── Change Points (minor, %d) ──%s\n", p.dim, len(minor), p.reset)
		p.writeChangePointRows(w, minor)
		_, _ = fmt.Fprintln(w)
	case len(minor) > 0:
		_, _ = fmt.Fprintf(w, "  %s(%d minor change points hidden, use -v to show)%s\n\n", p.dim, len(minor), p.reset)
	}

}

// writeOscillatingJobs summarizes jobs with 3+ change points — these are volatile, not changing.
func (p *palette) writeOscillatingJobs(w io.Writer, findings []analyze.Finding) {
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

	_, _ = fmt.Fprintf(w, "%s── Oscillating Jobs (%d jobs, too volatile for reliable change detection) ──%s\n", p.dim, len(summaries), p.reset)
	for _, s := range summaries {
		icon := p.yellow + "~" + p.reset
		trend := p.dim + "stable" + p.reset
		if s.current > s.earliest+s.earliest/10 {
			trend = p.red + "trending up" + p.reset
		} else if s.current < s.earliest-s.earliest/10 {
			trend = p.green + "trending down" + p.reset
		}
		_, _ = fmt.Fprintf(w, "  %s %s  %s%d shifts%s, was %s now %s (%s)\n",
			icon, s.name, p.dim, s.count, p.reset,
			fmtDur(s.earliest), fmtDur(s.current), trend)
	}
	_, _ = fmt.Fprintln(w)
}

func (p *palette) writeChangePointRows(w io.Writer, findings []analyze.Finding) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.StripEscape)
	_, _ = fmt.Fprintf(tw, "  %sDIR\tJOB\tCHANGE\tBEFORE\tAFTER\tDATE\tCOMMIT\tP-VALUE\tSTATUS%s\n",
		p.dim, p.reset)
	for _, f := range findings {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok {
			continue
		}

		var icon, changeColor string
		if d.Direction == analyze.DirectionSpeedup {
			icon = esc(p.green) + "▼" + esc(p.reset)
			changeColor = p.green
		} else {
			icon = esc(p.red) + "▲" + esc(p.reset)
			changeColor = p.red
		}

		status := p.formatPersistence(&d)

		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s%s%s\t%s\t%s\t%s\t%s%s%s\t%s\t%s\n",
			icon, d.JobName,
			esc(changeColor), fmt.Sprintf("%+.0f%%", d.PctChange), esc(p.reset),
			fmtDur(d.BeforeMean), fmtDur(d.AfterMean),
			d.Date.Format("2006-01-02"),
			esc(p.dim), truncSHA(d.CommitSHA), esc(p.reset),
			p.fmtPValueStr(d.PValue),
			status)
	}
	_ = tw.Flush()
}

// esc wraps an ANSI code in tabwriter escape markers so it's not counted for column width.
func esc(code string) string {
	return "\xff" + code + "\xff"
}

func (p *palette) fmtPValueStr(pval float64) string {
	s := fmt.Sprintf("%.4f", pval)
	var color string
	switch {
	case pval < 0.01:
		color = p.green
	case pval < 0.05:
		color = p.yellow
	default:
		color = p.dim
	}
	return esc(color) + s + esc(p.reset)
}

func (p *palette) formatPersistence(d *analyze.ChangePointDetail) string {
	switch d.Persistence {
	case analyze.PersistencePersistent:
		return fmt.Sprintf("%s✓ %d runs%s", esc(p.green), d.PostChangeRuns, esc(p.reset))
	case analyze.PersistenceTransient:
		return fmt.Sprintf("%stransient (%d runs)%s", esc(p.yellow), d.PostChangeRuns, esc(p.reset))
	case analyze.PersistenceInconclusive:
		return fmt.Sprintf("%s? %d runs%s", esc(p.dim), d.PostChangeRuns, esc(p.reset))
	default:
		return ""
	}
}

// severityDot returns a colored dot. Single visible char so alignment is consistent.
func (p *palette) severityDot(severity string) string {
	switch severity {
	case analyze.SeverityCritical:
		return p.red + "●" + p.reset
	case analyze.SeverityWarning:
		return p.yellow + "●" + p.reset
	default:
		return p.dim + "●" + p.reset
	}
}

func (p *palette) writeLegend(w io.Writer) {
	_, _ = fmt.Fprintf(w, "\n%s", p.dim)
	_, _ = fmt.Fprint(w, "Volatility (p95/median): [variable] 1.3-2x  [spiky] 2-3x  [volatile] >3x\n")
	_, _ = fmt.Fprintf(w, "Outliers: %s●%s critical (p99+)  %s●%s warning (p95+)  %s●%s info\n",
		p.red, p.dim, p.yellow, p.dim, p.dim, p.dim)
	_, _ = fmt.Fprint(w, "Change points: ▲ slowdown  ▼ speedup | Status: N runs = persistent, transient = reverted, ? = too few runs\n")
	_, _ = fmt.Fprintf(w, "%s", p.reset)
}
