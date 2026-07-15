package output

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/vertti/ci-snitch/internal/analyze"
)

// groupedFindings holds findings split by type for rendering.
type groupedFindings struct {
	Summaries    []analyze.Finding
	Steps        []analyze.Finding
	Pipelines    []analyze.Finding
	Runners      []analyze.Finding
	Outliers     []analyze.Finding
	Changepoints []analyze.Finding
	Failures     []analyze.Finding
	Costs        []analyze.Finding
}

// groupByType splits findings into typed buckets.
func groupByType(findings []analyze.Finding) groupedFindings {
	var g groupedFindings
	for _, f := range findings {
		switch f.Type {
		case analyze.TypeSummary:
			g.Summaries = append(g.Summaries, f)
		case analyze.TypeSteps:
			g.Steps = append(g.Steps, f)
		case analyze.TypePipeline:
			g.Pipelines = append(g.Pipelines, f)
		case analyze.TypeRunner:
			g.Runners = append(g.Runners, f)
		case analyze.TypeOutlier:
			g.Outliers = append(g.Outliers, f)
		case analyze.TypeChangepoint:
			g.Changepoints = append(g.Changepoints, f)
		case analyze.TypeFailure:
			g.Failures = append(g.Failures, f)
		case analyze.TypeCost:
			g.Costs = append(g.Costs, f)
		}
	}
	return g
}

// fmtDur formats a duration as a compact human-readable string (e.g. "5m30s",
// "2h30m" — a 2.5-hour p95 as "150m" is unreadable).
func fmtDur(ad analyze.Duration) string {
	d := ad.Std().Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d >= time.Hour {
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh%dm", h, m)
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}

// fmtPercentile floors for display: "p100 — slower than 100% of runs" would
// include the run itself.
func fmtPercentile(p float64) string {
	return fmt.Sprintf("p%.0f", math.Floor(p))
}

// escMD escapes pipes so names can't break markdown table rows.
func escMD(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

// fmtTotalTime formats a duration as hours and minutes (e.g. "2h30m").
func fmtTotalTime(ad analyze.Duration) string {
	d := ad.Std()
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// truncSHA returns the first 8 characters of a git SHA.
func truncSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
