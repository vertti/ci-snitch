package output

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/analyze"
)

func dur(d time.Duration) analyze.Duration { return analyze.Duration(d) }

func testResult() *analyze.AnalysisResult {
	return &analyze.AnalysisResult{
		Findings: []analyze.Finding{
			{
				Type: "summary", Severity: analyze.SeverityInfo,
				Title: "Workflow \"CI\" summary",
				Detail: analyze.SummaryDetail{
					Workflow: "CI",
					Stats: analyze.SummaryStats{
						TotalRuns: 50, Mean: dur(5 * time.Minute), Median: dur(5 * time.Minute),
						P95: dur(7 * time.Minute), P99: dur(8 * time.Minute),
						Min: dur(3 * time.Minute), Max: dur(10 * time.Minute),
						TotalTime: dur(250 * time.Minute),
					},
					Jobs: []analyze.JobSummary{
						{Name: "build", Stats: analyze.SummaryStats{TotalRuns: 50, Median: dur(3 * time.Minute), P95: dur(4 * time.Minute), Min: dur(2 * time.Minute), Max: dur(6 * time.Minute)}},
						{Name: "test", Stats: analyze.SummaryStats{TotalRuns: 50, Median: dur(2 * time.Minute), P95: dur(3 * time.Minute), Min: dur(1 * time.Minute), Max: dur(4 * time.Minute)}},
					},
				},
			},
			{
				Type: "outlier", Severity: analyze.SeverityWarning,
				Title:       "Slow run in \"CI\"",
				Description: "Run took 10m (p97)",
				Detail: analyze.OutlierDetail{
					RunID: 123, CommitSHA: "aabbccdd11223344",
					Duration: dur(10 * time.Minute), Percentile: 97,
					WorkflowName: "CI",
				},
			},
			{
				Type: "changepoint", Severity: analyze.SeverityWarning,
				Title:       "Performance slowdown in job \"build\"",
				Description: "+25% change at 2026-04-01 (commit aabbccdd), before: 5m, after: 6m15s (p=0.0300)",
				Detail: analyze.ChangePointDetail{
					JobName: "build", ChangeIdx: 20,
					BeforeMean: dur(5 * time.Minute), AfterMean: dur(6*time.Minute + 15*time.Second),
					PctChange: 25, Direction: analyze.DirectionSlowdown,
					PValue: 0.03, CommitSHA: "aabbccdd11223344",
					Date: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
				},
			},
		},
		Meta: analyze.ResultMeta{
			TotalRuns:   50,
			TimeRange:   [2]time.Time{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
			WorkflowIDs: []int64{100},
		},
	}
}

func TestGet(t *testing.T) {
	opts := Options{}

	f, ok := Get("json", opts)
	assert.IsType(t, JSONFormatter{}, f)
	assert.True(t, ok)

	f, ok = Get("table", opts)
	assert.IsType(t, TableFormatter{}, f)
	assert.True(t, ok)

	f, ok = Get("markdown", opts)
	assert.IsType(t, MarkdownFormatter{}, f)
	assert.True(t, ok)

	f, ok = Get("md", opts)
	assert.IsType(t, MarkdownFormatter{}, f)
	assert.True(t, ok)

	f, ok = Get("llm", opts)
	assert.IsType(t, LLMFormatter{}, f)
	assert.True(t, ok)

	f, ok = Get("unknown", opts)
	assert.IsType(t, TableFormatter{}, f)
	assert.False(t, ok)
}

func TestLLMFormatter(t *testing.T) {
	var buf bytes.Buffer
	err := LLMFormatter{}.Format(&buf, testResult())
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "# CI Performance Report")
	assert.Contains(t, out, "## Priority Findings")
	assert.Contains(t, out, "## Workflow Summaries")
	assert.Contains(t, out, "## Raw Data")
	assert.Contains(t, out, "```json")
}

func TestJSONFormatter(t *testing.T) {
	var buf bytes.Buffer
	err := JSONFormatter{}.Format(&buf, testResult())
	require.NoError(t, err)

	var parsed map[string]any
	err = json.Unmarshal(buf.Bytes(), &parsed)
	require.NoError(t, err, "output should be valid JSON")

	findings, ok := parsed["findings"].([]any)
	require.True(t, ok)
	assert.Len(t, findings, 3)
}

func TestTableFormatter(t *testing.T) {
	var buf bytes.Buffer
	err := TableFormatter{}.Format(&buf, testResult())
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "CI")
	assert.Contains(t, out, "50 runs")
	assert.Contains(t, out, "build")
	assert.Contains(t, out, "test")
	assert.Contains(t, out, "Outliers")
	assert.Contains(t, out, "Change Points")
	assert.Contains(t, out, "50 runs analyzed")
}

func TestTableFormatter_Empty(t *testing.T) {
	var buf bytes.Buffer
	err := TableFormatter{}.Format(&buf, &analyze.AnalysisResult{})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "No findings")
}

func TestMarkdownFormatter(t *testing.T) {
	var buf bytes.Buffer
	err := MarkdownFormatter{}.Format(&buf, richTestResult())
	require.NoError(t, err)

	out := buf.String()

	// Summary section
	assert.Contains(t, out, "# CI Performance Report")
	assert.Contains(t, out, "**50 runs**")
	assert.Contains(t, out, "### CI")
	assert.Contains(t, out, "| build |")

	// Changepoint section renders arrow icons, not literal direction strings
	assert.Contains(t, out, "## Performance Changes")
	assert.Contains(t, out, "▲")
	assert.NotContains(t, out, "**slowdown")

	// Outlier section renders grouped data with table rows
	assert.Contains(t, out, "## Outliers (2 groups)")
	assert.Contains(t, out, "| Count |")
	assert.Contains(t, out, "| critical | CI / build | 5 | 15m")
	assert.Contains(t, out, "p99")
}

func TestCompactResult_FiltersNoise(t *testing.T) {
	result := analyze.AnalysisResult{
		Findings: []analyze.Finding{
			{Type: analyze.TypeSummary, Detail: analyze.SummaryDetail{Workflow: "CI"}},
			{Type: analyze.TypeChangepoint, Detail: analyze.ChangePointDetail{Category: analyze.CategoryRegression}},
			{Type: analyze.TypeChangepoint, Detail: analyze.ChangePointDetail{Category: analyze.CategorySpeedup}},
			{Type: analyze.TypeChangepoint, Detail: analyze.ChangePointDetail{Category: analyze.CategoryOscillating}},
			{Type: analyze.TypeChangepoint, Detail: analyze.ChangePointDetail{Category: analyze.CategoryMinor}},
			{Type: analyze.TypeChangepoint, Detail: analyze.ChangePointDetail{Category: analyze.CategoryMinor}},
			{Type: analyze.TypeFailure, Detail: analyze.FailureDetail{Workflow: "CI"}},
			{Type: analyze.TypeCost, Detail: analyze.CostDetail{Workflow: "CI"}},
		},
		Meta: analyze.ResultMeta{TotalRuns: 100},
	}

	compact := compactResult(&result)

	// Should keep: summary, 2 actionable changepoints, failure, cost = 5
	// Should drop: 1 oscillating + 2 minor = 3
	assert.Len(t, compact.Findings, 5)
	assert.Equal(t, 100, compact.Meta.TotalRuns)

	// Verify no oscillating or minor changepoints remain
	for _, f := range compact.Findings {
		if f.Type == analyze.TypeChangepoint {
			d, ok := f.Detail.(analyze.ChangePointDetail)
			require.True(t, ok)
			assert.NotEqual(t, analyze.CategoryOscillating, d.Category)
			assert.NotEqual(t, analyze.CategoryMinor, d.Category)
		}
	}
}

func richTestResult() *analyze.AnalysisResult {
	base := testResult()
	base.Findings = append(base.Findings,
		analyze.Finding{
			Type: analyze.TypeCost, Severity: analyze.SeverityInfo,
			Title: "Workflow \"CI\" cost",
			Detail: analyze.CostDetail{
				Workflow: "CI", TotalRuns: 50, BillableMinutes: 500,
				DailyRate: 50, PriorityScore: 100, DailySavingsEstimate: 5,
				Jobs: []analyze.JobCostBreakdown{
					{Name: "build", BillableMinutes: 300, Multiplier: 1, Runs: 50},
					{Name: "test", BillableMinutes: 200, Multiplier: 1, Runs: 50},
				},
			},
		},
		analyze.Finding{
			Type: analyze.TypeFailure, Severity: analyze.SeverityWarning,
			Title: "Workflow \"CI\" failure rate",
			Detail: analyze.FailureDetail{
				Workflow: "CI", TotalRuns: 100, FailureCount: 15, FailureRate: 0.15,
				ByConclusion: map[string]int{"failure": 10, "cancelled": 5},
				RetriedRuns:  3, ExtraAttempts: 4,
			},
		},
		analyze.Finding{
			Type: analyze.TypeOutlier, Severity: analyze.SeverityCritical,
			Title: "Outliers in CI / build",
			Detail: analyze.OutlierGroupDetail{
				WorkflowName: "CI", JobName: "build", Count: 5,
				WorstDuration: dur(15 * time.Minute), WorstPercentile: 99,
				WorstCommitSHA: "aabbccdd11223344", MaxSeverity: analyze.SeverityCritical,
			},
		},
	)
	return base
}

func TestTableFormatter_AllSections(t *testing.T) {
	var buf bytes.Buffer
	err := TableFormatter{}.Format(&buf, richTestResult())
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Triage")
	assert.Contains(t, out, "CI Cost")
	assert.Contains(t, out, "500 mins")
	assert.Contains(t, out, "Failure Rates")
	assert.Contains(t, out, "15%")
	assert.Contains(t, out, "Outliers")
	assert.Contains(t, out, "5x")
	assert.Contains(t, out, "Change Points")
	assert.Contains(t, out, "build")
	assert.Contains(t, out, "Volatility")
}

func TestMarkdownFormatter_SpeedupArrow(t *testing.T) {
	result := analyze.AnalysisResult{
		Findings: []analyze.Finding{
			{
				Type: "changepoint", Severity: analyze.SeverityWarning,
				Detail: analyze.ChangePointDetail{
					JobName: "deploy", ChangeIdx: 10,
					BeforeMean: dur(10 * time.Minute), AfterMean: dur(7 * time.Minute),
					PctChange: -30, Direction: analyze.DirectionSpeedup,
					PValue: 0.01, CommitSHA: "11223344aabbccdd",
					Date: time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
				},
			},
		},
		Meta: analyze.ResultMeta{
			TotalRuns: 50,
			TimeRange: [2]time.Time{time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		},
	}

	var buf bytes.Buffer
	err := MarkdownFormatter{}.Format(&buf, &result)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "▼")
	assert.NotContains(t, out, "speedup")
}

func TestLLMFormatter_AllSections(t *testing.T) {
	var buf bytes.Buffer
	err := LLMFormatter{}.Format(&buf, richTestResult())
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Priority Findings")
	assert.Contains(t, out, "[FLAKY]")
	assert.Contains(t, out, "[COST]")
	assert.Contains(t, out, "Workflow Summaries")
	assert.Contains(t, out, "Raw Data")
	assert.Contains(t, out, "Suggested Investigations")
}

func TestGroupByType(t *testing.T) {
	findings := []analyze.Finding{
		{Type: analyze.TypeSummary},
		{Type: analyze.TypeSummary},
		{Type: analyze.TypeOutlier},
		{Type: analyze.TypeChangepoint},
		{Type: analyze.TypeFailure},
		{Type: analyze.TypeCost},
		{Type: analyze.TypeCost},
		{Type: analyze.TypeSteps},
	}

	g := groupByType(findings)
	assert.Len(t, g.Summaries, 2)
	assert.Len(t, g.Outliers, 1)
	assert.Len(t, g.Changepoints, 1)
	assert.Len(t, g.Failures, 1)
	assert.Len(t, g.Costs, 2)
	assert.Len(t, g.Steps, 1)
}

func TestTruncSHA(t *testing.T) {
	assert.Equal(t, "aabbccdd", truncSHA("aabbccdd11223344"))
	assert.Equal(t, "short", truncSHA("short"))
	assert.Empty(t, truncSHA(""))
}

func TestFmtTotalTime(t *testing.T) {
	assert.Equal(t, "30m", fmtTotalTime(dur(30*time.Minute)))
	assert.Equal(t, "2h30m", fmtTotalTime(dur(150*time.Minute)))
	assert.Equal(t, "0m", fmtTotalTime(0))
}

func TestTableFormatter_StepTable(t *testing.T) {
	result := analyze.AnalysisResult{
		Findings: []analyze.Finding{
			{
				Type: analyze.TypeSteps, Severity: analyze.SeverityInfo,
				Detail: analyze.StepTimingDetail{
					WorkflowName: "CI", JobName: "build", TotalRuns: 50,
					Steps: []analyze.StepSummary{
						{Name: "Checkout", Runs: 50, Median: dur(5 * time.Second), P95: dur(8 * time.Second), PctOfJob: 3, Volatility: 1.2},
						{Name: "Build", Runs: 50, Median: dur(2 * time.Minute), P95: dur(3 * time.Minute), PctOfJob: 60, Volatility: 2.5},
						{Name: "Test", Runs: 50, Median: dur(1 * time.Minute), P95: dur(2 * time.Minute), PctOfJob: 30, Volatility: 1.1},
					},
				},
			},
		},
		Meta: analyze.ResultMeta{TotalRuns: 50, TimeRange: [2]time.Time{time.Now(), time.Now()}},
	}

	var buf bytes.Buffer
	err := TableFormatter{}.Format(&buf, &result)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Step Breakdown")
	assert.Contains(t, out, "Checkout")
	assert.Contains(t, out, "Build")
	assert.Contains(t, out, "60% of job")
	assert.Contains(t, out, "[2.5x]") // volatile step marker
}

func TestTableFormatter_OscillatingJobs(t *testing.T) {
	result := analyze.AnalysisResult{
		Findings: []analyze.Finding{
			{Type: analyze.TypeChangepoint, Severity: analyze.SeverityWarning, Detail: analyze.ChangePointDetail{
				JobName: "test", Direction: analyze.DirectionSlowdown, Category: analyze.CategoryOscillating,
				BeforeMean: dur(5 * time.Minute), AfterMean: dur(7 * time.Minute),
			}},
			{Type: analyze.TypeChangepoint, Severity: analyze.SeverityWarning, Detail: analyze.ChangePointDetail{
				JobName: "test", Direction: analyze.DirectionSpeedup, Category: analyze.CategoryOscillating,
				BeforeMean: dur(7 * time.Minute), AfterMean: dur(5 * time.Minute),
			}},
			{Type: analyze.TypeChangepoint, Severity: analyze.SeverityWarning, Detail: analyze.ChangePointDetail{
				JobName: "test", Direction: analyze.DirectionSlowdown, Category: analyze.CategoryOscillating,
				BeforeMean: dur(5 * time.Minute), AfterMean: dur(8 * time.Minute),
			}},
		},
		Meta: analyze.ResultMeta{TotalRuns: 50, TimeRange: [2]time.Time{time.Now(), time.Now()}},
	}

	var buf bytes.Buffer
	err := TableFormatter{}.Format(&buf, &result)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Oscillating Jobs")
	assert.Contains(t, out, "test")
	assert.Contains(t, out, "3 shifts")
	assert.Contains(t, out, "trending up")
}

func TestTableFormatter_ChangePointPersistence(t *testing.T) {
	result := analyze.AnalysisResult{
		Findings: []analyze.Finding{
			{Type: analyze.TypeChangepoint, Severity: analyze.SeverityWarning, Detail: analyze.ChangePointDetail{
				JobName: "build", Direction: analyze.DirectionSlowdown, Category: analyze.CategoryRegression,
				BeforeMean: dur(3 * time.Minute), AfterMean: dur(4 * time.Minute),
				PctChange: 33, PValue: 0.001, CommitSHA: "aabbccdd11223344",
				Date:        time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
				Persistence: "persistent", PostChangeRuns: 30,
			}},
			{Type: analyze.TypeChangepoint, Severity: analyze.SeverityWarning, Detail: analyze.ChangePointDetail{
				JobName: "deploy", Direction: analyze.DirectionSpeedup, Category: analyze.CategorySpeedup,
				BeforeMean: dur(10 * time.Minute), AfterMean: dur(7 * time.Minute),
				PctChange: -30, PValue: 0.05, CommitSHA: "11223344aabbccdd",
				Date:        time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
				Persistence: "transient", PostChangeRuns: 15,
			}},
			{Type: analyze.TypeChangepoint, Severity: analyze.SeverityInfo, Detail: analyze.ChangePointDetail{
				JobName: "lint", Direction: analyze.DirectionSlowdown, Category: analyze.CategoryMinor,
				BeforeMean: dur(30 * time.Second), AfterMean: dur(32 * time.Second),
				PctChange: 7, PValue: 0.2, CommitSHA: "ccddaabb11223344",
				Date:        time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC),
				Persistence: "inconclusive", PostChangeRuns: 3,
			}},
		},
		Meta: analyze.ResultMeta{TotalRuns: 50, TimeRange: [2]time.Time{time.Now(), time.Now()}},
	}

	var buf bytes.Buffer
	err := TableFormatter{Verbose: true}.Format(&buf, &result)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "30 runs")              // persistent
	assert.Contains(t, out, "transient")            // transient label
	assert.Contains(t, out, "? 3 runs")             // inconclusive
	assert.Contains(t, out, "Change Points (minor") // verbose shows minor
}

func TestFmtVolatility(t *testing.T) {
	assert.Empty(t, fmtVolatility("stable"))
	assert.Contains(t, fmtVolatility("variable"), "variable")
	assert.Contains(t, fmtVolatility("spiky"), "spiky")
	assert.Contains(t, fmtVolatility("volatile"), "volatile")
}

func TestFmtDur(t *testing.T) {
	tests := []struct {
		dur  analyze.Duration
		want string
	}{
		{dur(30 * time.Second), "30s"},
		{dur(90 * time.Second), "1m30s"},
		{dur(5 * time.Minute), "5m"},
		{dur(5*time.Minute + 30*time.Second), "5m30s"},
		{0, "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, fmtDur(tt.dur))
		})
	}
}
