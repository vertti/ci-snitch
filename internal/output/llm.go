package output

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/vertti/ci-snitch/internal/analyze"
	"github.com/vertti/ci-snitch/internal/diag"
)

// LLMFormatter produces structured output optimized for LLM consumption.
// Combines narrative context with raw JSON data.
type LLMFormatter struct {
	RawOutputPath string // if set, write full JSON to file instead of embedding
}

// Format implements Formatter.
func (l LLMFormatter) Format(w io.Writer, result *analyze.AnalysisResult) error {
	repo := result.Meta.Repo
	from := result.Meta.TimeRange[0].Format("2006-01-02")
	to := result.Meta.TimeRange[1].Format("2006-01-02")

	_, _ = fmt.Fprintf(w, "# CI Performance Report: %s\nAnalyzed %d runs, %s to %s.\n\n",
		repo, result.Meta.TotalRuns, from, to)

	g := groupByType(result.Findings)

	llmWritePriorityFindings(w, &g)
	llmWriteSummaryTable(w, g.Summaries)
	llmWritePipelines(w, g.Pipelines)
	llmWriteRunners(w, g.Runners)
	llmWriteSteps(w, g.Steps)

	suggestions := buildSuggestions(g.Changepoints, g.Failures, g.Costs, g.Outliers, g.Steps, g.Pipelines)
	if len(suggestions) > 0 {
		_, _ = fmt.Fprint(w, "\n## Suggested Investigations\n\n")
		for _, s := range suggestions {
			_, _ = fmt.Fprintf(w, "- %s\n", s)
		}
	}

	llmWriteCaveats(w, result.Diagnostics)
	llmWriteGlossary(w)

	return l.writeRawData(w, result)
}

// llmWriteCaveats narrates data-quality diagnostics — an LLM acting on the
// numbers must know when the dataset was truncated or partially fetched.
func llmWriteCaveats(w io.Writer, diags []diag.Diagnostic) {
	if len(diags) == 0 {
		return
	}
	_, _ = fmt.Fprint(w, "\n## Data Caveats\n\n")
	for _, d := range diags {
		_, _ = fmt.Fprintf(w, "- %s\n", d.String())
	}
}

func llmWriteGlossary(w io.Writer) {
	_, _ = fmt.Fprint(w, `
## Glossary

- volatility: p95/median duration ratio per series — stable <1.3, variable 1.3-2x, spiky 2-3x, volatile >=3x
- persistence: persistent = the shift held for the rest of the window; transient = it reverted; inconclusive = too few runs after the change to judge
- q_value: false-discovery-rate adjusted p-value across all change points in this report; treat q > 0.05 as noise
- billable minutes: per-job wall clock rounded up to whole minutes times the runner multiplier; self-hosted runners bill 0
`)
}

func llmWritePriorityFindings(w io.Writer, g *groupedFindings) {
	_, _ = fmt.Fprint(w, "## Priority Findings\n\n")
	hasPriority := false

	for _, f := range g.Changepoints {
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

	for _, f := range g.Failures {
		d, ok := f.Detail.(analyze.FailureDetail)
		if !ok {
			continue
		}
		hasPriority = true
		tag := "FLAKY"
		if d.FailureKind == analyze.FailureKindSystematic {
			tag = "SYSTEMATIC"
		}
		trendNote := ""
		switch d.Trend {
		case analyze.FailureTrendImproving:
			trendNote = fmt.Sprintf(", **improving** (recent 7d: %.0f%%)", d.RecentFailureRate*100)
		case analyze.FailureTrendWorsening:
			trendNote = fmt.Sprintf(", **worsening** (recent 7d: %.0f%%)", d.RecentFailureRate*100)
		}
		_, _ = fmt.Fprintf(w, "- **[%s]** %s: %.0f%% failure rate (%d/%d runs)%s",
			tag, d.Workflow, d.FailureRate*100, d.FailureCount, d.TotalRuns, trendNote)
		_, _ = fmt.Fprint(w, failingStepHeadline(&d))
		if len(d.ByCategory) > 1 {
			_, _ = fmt.Fprint(w, categoryBreakdown(&d))
		}
		if d.RetriedRuns > 0 {
			_, _ = fmt.Fprintf(w, ", %d retried (+%d extra attempts)", d.RetriedRuns, d.ExtraAttempts)
		}
		_, _ = fmt.Fprint(w, "\n")
	}

	costShown := 0
	for i := range g.Costs {
		if costShown == 3 {
			break
		}
		d, ok := g.Costs[i].Detail.(analyze.CostDetail)
		// Same bar as the suggestions section: a negligible-score workflow
		// is not a priority finding.
		if !ok || d.PriorityScore < 50 {
			continue
		}
		costShown++
		hasPriority = true
		_, _ = fmt.Fprintf(w, "- **[COST]** %s: %.0f billable mins/day (%.0f total)\n",
			d.Workflow, d.DailyRate, d.BillableMinutes)
	}

	if !hasPriority {
		_, _ = fmt.Fprint(w, "No critical findings.\n")
	}
}

func llmWriteSummaryTable(w io.Writer, summaries []analyze.Finding) {
	_, _ = fmt.Fprint(w, "\n## Workflow Summaries\n\n")
	_, _ = fmt.Fprint(w, "| Workflow | Runs | Median | P95 | Queue | Total | Volatility |\n")
	_, _ = fmt.Fprint(w, "|----------|------|--------|-----|-------|-------|------------|\n")
	for _, f := range summaries {
		d, ok := f.Detail.(analyze.SummaryDetail)
		if !ok {
			continue
		}
		queueStr := "-"
		if d.Queue.Median.Std() > 0 {
			queueStr = fmtDur(d.Queue.Median)
		}
		_, _ = fmt.Fprintf(w, "| %s | %d | %s | %s | %s | %s | %s |\n",
			escMD(d.Workflow), d.Stats.TotalRuns,
			fmtDur(d.Stats.Median), fmtDur(d.Stats.P95),
			queueStr,
			fmtTotalTime(d.Stats.TotalTime), d.Stats.VolatilityLabel)
	}
}

func llmWritePipelines(w io.Writer, pipelines []analyze.Finding) {
	if len(pipelines) == 0 {
		return
	}
	_, _ = fmt.Fprint(w, "\n## Pipeline Structure\n\n")
	for _, f := range pipelines {
		d, ok := f.Detail.(analyze.PipelineDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "**%s** — %.0f%% parallel efficiency, wall-clock %s, total job time %s\n",
			d.Workflow, d.Parallelism*100, fmtDur(d.MedianWallClock), fmtDur(d.MedianJobSum))
		for _, stage := range d.Stages {
			marker := ""
			if stage.Name == d.CriticalPath {
				marker = " [critical]"
			}
			if stage.Sequential {
				marker += " [seq]"
			}
			_, _ = fmt.Fprintf(w, "- %s: %s (%.0f%%, %d jobs)%s\n",
				stage.Name, fmtDur(stage.Duration), stage.PctOfPipeline, len(stage.Jobs), marker)
		}
		_, _ = fmt.Fprint(w, "\n")
	}
}

func llmWriteRunners(w io.Writer, runners []analyze.Finding) {
	if len(runners) == 0 {
		return
	}
	_, _ = fmt.Fprint(w, "\n## Runner Sizing\n\n")
	for _, f := range runners {
		d, ok := f.Detail.(analyze.RunnerDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "- [%s] %s / %s (%s, %d cores): %s\n",
			d.Issue, d.WorkflowName, d.JobName, d.RunnerLabel, d.Cores, d.Suggestion)
	}
}

func llmWriteSteps(w io.Writer, steps []analyze.Finding) {
	if len(steps) == 0 {
		return
	}
	_, _ = fmt.Fprint(w, "\n## Step-Level Timing\n\n")
	shown := min(5, len(steps))
	for _, f := range steps[:shown] {
		d, ok := f.Detail.(analyze.StepTimingDetail)
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "**%s / %s** (%d runs):\n", d.WorkflowName, d.JobName, d.TotalRuns)
		for _, st := range d.Steps {
			_, _ = fmt.Fprintf(w, "- %s: median %s, p95 %s (%.0f%% of job)",
				st.Name, fmtDur(st.Median), fmtDur(st.P95), st.PctOfJob)
			if st.Volatility >= 2.0 {
				_, _ = fmt.Fprintf(w, " [%.1fx volatile]", st.Volatility)
			}
			_, _ = fmt.Fprint(w, "\n")
		}
		_, _ = fmt.Fprint(w, "\n")
	}
}

func (l LLMFormatter) writeRawData(w io.Writer, result *analyze.AnalysisResult) error {
	compact := compactResult(result)
	omitted := len(result.Findings) - len(compact.Findings)

	if l.RawOutputPath != "" {
		if err := writeJSONFile(l.RawOutputPath, result); err != nil {
			return fmt.Errorf("write raw output: %w", err)
		}
		_, _ = fmt.Fprintf(w, "\n## Raw Data\n\nFull JSON written to `%s` (%d findings).\n", l.RawOutputPath, len(result.Findings))
		if omitted > 0 {
			_, _ = fmt.Fprintf(w, "(%d oscillating/minor changepoints omitted from report; included in file)\n", omitted)
		}
	} else {
		_, _ = fmt.Fprintf(w, "\n## Raw Data (%d findings)\n\n", len(compact.Findings))
		if omitted > 0 {
			_, _ = fmt.Fprintf(w, "(%d oscillating/minor changepoints filtered)\n\n", omitted)
		}
		_, _ = fmt.Fprint(w, "```json\n")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(compact); err != nil {
			return fmt.Errorf("encode JSON: %w", err)
		}
		_, _ = fmt.Fprint(w, "```\n")
	}

	return nil
}

func writeJSONFile(path string, result *analyze.AnalysisResult) error {
	f, err := os.Create(path) //nolint:gosec // path comes from user's --raw-output flag
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // best-effort close on write path
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// compactResult strips noise from the analysis result for LLM consumption.
// Drops oscillating/minor changepoints, which inflate the JSON from ~54k
// tokens to ~5k without adding actionable information (other finding types
// pass through unchanged).
func compactResult(result *analyze.AnalysisResult) analyze.AnalysisResult {
	var filtered []analyze.Finding
	for _, f := range result.Findings {
		switch f.Type {
		case analyze.TypeChangepoint:
			d, ok := f.Detail.(analyze.ChangePointDetail)
			if !ok {
				continue
			}
			if d.Category == analyze.CategoryOscillating || d.Category == analyze.CategoryMinor {
				continue
			}
			filtered = append(filtered, f)
		default:
			filtered = append(filtered, f)
		}
	}
	return analyze.AnalysisResult{
		Findings:    filtered,
		Diagnostics: result.Diagnostics,
		Meta:        result.Meta,
	}
}

// failingStepHeadline summarizes the failure pattern.
// Single dominant step: "fails at step X in job Y (N/M failures)"
// Distributed failures: "failures spread across N steps (top: X Nx, Y Nx)"
func failingStepHeadline(d *analyze.FailureDetail) string {
	if len(d.FailingSteps) == 0 {
		return ""
	}
	top := d.FailingSteps[0]
	// Single dominant step: top step accounts for >60% of failures
	if len(d.FailingSteps) == 1 || float64(top.Count) > float64(d.FailureCount)*0.6 {
		s := fmt.Sprintf(" -- fails at step %q", top.StepName)
		if top.JobName != "" {
			s += fmt.Sprintf(" in job %q", top.JobName)
		}
		s += fmt.Sprintf(" (%d/%d failures)", top.Count, d.FailureCount)
		return s
	}
	// Distributed: show count of steps + top 2
	s := fmt.Sprintf(" -- failures spread across %d steps (top: %q %dx",
		len(d.FailingSteps), top.StepName, top.Count)
	if len(d.FailingSteps) > 1 {
		s += fmt.Sprintf(", %q %dx", d.FailingSteps[1].StepName, d.FailingSteps[1].Count)
	}
	s += ")"
	return s
}

// categoryBreakdown returns a compact summary like " (40% infra, 35% build, 25% test)".
func categoryBreakdown(d *analyze.FailureDetail) string {
	total := 0
	for _, count := range d.ByCategory {
		total += count
	}
	if total == 0 {
		return ""
	}

	// Sort categories by count descending
	type catCount struct {
		name  string
		count int
	}
	var cats []catCount
	for name, count := range d.ByCategory {
		cats = append(cats, catCount{name, count})
	}
	slices.SortFunc(cats, func(a, b catCount) int {
		if a.count != b.count {
			return b.count - a.count
		}
		return cmp.Compare(a.name, b.name) // deterministic order on ties
	})

	var parts []string
	for _, c := range cats {
		pct := float64(c.count) / float64(total) * 100
		if pct >= 5 { // skip tiny categories
			parts = append(parts, fmt.Sprintf("%.0f%% %s", pct, c.name))
		}
	}
	if len(parts) <= 1 {
		return "" // single category, not interesting
	}
	return fmt.Sprintf(" (%s)", strings.Join(parts, ", "))
}

func buildSuggestions(changepoints, failures, costs, outliers, steps, pipelines []analyze.Finding) []string {
	volatileSteps := buildVolatileStepIndex(steps)
	var suggestions []string
	suggestions = append(suggestions, suggestFromChangepoints(changepoints)...)
	suggestions = append(suggestions, suggestFromPipelines(pipelines)...)
	suggestions = append(suggestions, suggestFromCosts(costs)...)
	suggestions = append(suggestions, suggestFromOutliers(outliers, volatileSteps)...)
	suggestions = append(suggestions, suggestFromFailures(failures)...)
	return suggestions
}

type volatileStep struct {
	name       string
	volatility float64
}

// wfJobKey scopes job lookups to their workflow — job names collide across
// workflows constantly ("build", "test").
type wfJobKey struct{ wf, job string }

func buildVolatileStepIndex(steps []analyze.Finding) map[wfJobKey]volatileStep {
	index := make(map[wfJobKey]volatileStep)
	for _, f := range steps {
		d, ok := f.Detail.(analyze.StepTimingDetail)
		if !ok {
			continue
		}
		k := wfJobKey{d.WorkflowName, d.JobName}
		for _, st := range d.Steps {
			if st.Volatility >= 2.0 {
				if existing, ok := index[k]; !ok || st.Volatility > existing.volatility {
					index[k] = volatileStep{st.Name, st.Volatility}
				}
			}
		}
	}
	return index
}

// suggestFromPipelines surfaces sequential stages whose parallelization
// upper bound is worth a look. Estimate only — dependencies must be checked.
func suggestFromPipelines(findings []analyze.Finding) []string {
	var s []string
	for _, f := range findings {
		d, ok := f.Detail.(analyze.PipelineDetail)
		if !ok {
			continue
		}
		for _, st := range d.Stages {
			if st.Sequential && st.PotentialSavings.Std() >= time.Minute {
				s = append(s, fmt.Sprintf("In %q, stage %q waits for the previous stage — running them in parallel could save up to ~%s per run (verify job dependencies first)",
					d.Workflow, st.Name, fmtDur(st.PotentialSavings)))
			}
		}
	}
	return s
}

func suggestFromChangepoints(findings []analyze.Finding) []string {
	var s []string
	for _, f := range findings {
		d, ok := f.Detail.(analyze.ChangePointDetail)
		if !ok || d.Category != analyze.CategoryRegression || d.Direction != analyze.DirectionSlowdown {
			continue
		}
		s = append(s, fmt.Sprintf("What changed in commit `%s` (%s) that affected %q?",
			truncSHA(d.CommitSHA), d.Date.Format("2006-01-02"), d.JobName))
	}
	return s
}

func suggestFromCosts(findings []analyze.Finding) []string {
	var s []string
	for _, f := range findings {
		d, ok := f.Detail.(analyze.CostDetail)
		if !ok || d.PriorityScore < 50 {
			continue
		}
		s = append(s, fmt.Sprintf("%q is a high-cost workflow (%.0f mins/day) -- check for flaky tests, cache misses, or resource contention",
			d.Workflow, d.DailyRate))
	}
	return s
}

func suggestFromOutliers(findings []analyze.Finding, volatileSteps map[wfJobKey]volatileStep) []string {
	var s []string
	for _, f := range findings {
		d, ok := f.Detail.(analyze.OutlierGroupDetail)
		if !ok || d.Count < 5 {
			continue
		}
		subject := d.WorkflowName
		if d.JobName != "" {
			subject = d.JobName
		}
		hint := "check for resource contention or flaky infrastructure"
		if vs, ok := volatileSteps[wfJobKey{d.WorkflowName, d.JobName}]; ok {
			hint = fmt.Sprintf("step %q is %.1fx volatile and likely the cause", vs.name, vs.volatility)
		}
		s = append(s, fmt.Sprintf("%q has %d outliers (worst %s) -- %s",
			subject, d.Count, fmtDur(d.WorstDuration), hint))
	}
	return s
}

func suggestFromFailures(findings []analyze.Finding) []string {
	var s []string
	for _, f := range findings {
		d, ok := f.Detail.(analyze.FailureDetail)
		if !ok || d.FailureRate < 0.1 {
			continue
		}
		var conclusionHint string
		maxConclusion := ""
		maxCount := 0
		for c, n := range d.ByConclusion {
			// Lexicographic tie-break: map order must not decide the hint.
			if n > maxCount || (n == maxCount && (maxConclusion == "" || c < maxConclusion)) {
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
		s = append(s, fmt.Sprintf("%q has %.0f%% failure rate%s",
			d.Workflow, d.FailureRate*100, conclusionHint))
	}
	return s
}
