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

func testResult() analyze.AnalysisResult {
	return analyze.AnalysisResult{
		Findings: []analyze.Finding{
			{
				Type: "summary", Severity: "info",
				Title: "Workflow \"CI\" summary",
				Detail: analyze.SummaryDetail{
					Subject: "CI", TotalRuns: 50,
					Mean: 5 * time.Minute, Median: 5 * time.Minute,
					P95: 7 * time.Minute, P99: 8 * time.Minute,
					Min: 3 * time.Minute, Max: 10 * time.Minute,
				},
			},
			{
				Type: "outlier", Severity: "warning",
				Title:       "Slow run in \"CI\"",
				Description: "Run took 10m (p97)",
				Detail: analyze.OutlierDetail{
					RunID: 123, CommitSHA: "aabbccdd11223344",
					Duration: 10 * time.Minute, Percentile: 97,
				},
			},
			{
				Type: "changepoint", Severity: "warning",
				Title:       "Performance slowdown in job \"build\"",
				Description: "+25% change at 2026-04-01 (commit aabbccdd), before: 5m, after: 6m15s (p=0.0300)",
				Detail: analyze.ChangePointDetail{
					JobName: "build", ChangeIdx: 20,
					BeforeMean: 5 * time.Minute, AfterMean: 6*time.Minute + 15*time.Second,
					PctChange: 25, Direction: "slowdown",
					PValue: 0.03, CommitSHA: "aabbccdd11223344",
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
	assert.IsType(t, JSONFormatter{}, Get("json", opts))
	assert.IsType(t, TableFormatter{}, Get("table", opts))
	assert.IsType(t, MarkdownFormatter{}, Get("markdown", opts))
	assert.IsType(t, MarkdownFormatter{}, Get("md", opts))
	assert.IsType(t, TableFormatter{}, Get("unknown", opts))
}

func TestJSONFormatter(t *testing.T) {
	var buf bytes.Buffer
	err := JSONFormatter{}.Format(&buf, testResult())
	require.NoError(t, err)

	var parsed map[string]any
	err = json.Unmarshal(buf.Bytes(), &parsed)
	require.NoError(t, err, "output should be valid JSON")

	findings, ok := parsed["Findings"].([]any)
	require.True(t, ok)
	assert.Len(t, findings, 3)
}

func TestTableFormatter(t *testing.T) {
	var buf bytes.Buffer
	err := TableFormatter{}.Format(&buf, testResult())
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Summary")
	assert.Contains(t, out, "CI")
	assert.Contains(t, out, "50")
	assert.Contains(t, out, "Outliers")
	assert.Contains(t, out, "Change Points")
	assert.Contains(t, out, "50 runs analyzed")
}

func TestTableFormatter_Empty(t *testing.T) {
	var buf bytes.Buffer
	err := TableFormatter{}.Format(&buf, analyze.AnalysisResult{})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "No findings")
}

func TestMarkdownFormatter(t *testing.T) {
	var buf bytes.Buffer
	err := MarkdownFormatter{}.Format(&buf, testResult())
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "# CI Performance Report")
	assert.Contains(t, out, "**50 runs**")
	assert.Contains(t, out, "## Summary")
	assert.Contains(t, out, "| CI |")
	assert.Contains(t, out, "## Performance Changes")
	assert.Contains(t, out, "## Outliers")
	// Markdown table headers
	assert.Contains(t, out, "|---")
}

func TestFmtDur(t *testing.T) {
	tests := []struct {
		dur  time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m30s"},
		{5 * time.Minute, "5m"},
		{5*time.Minute + 30*time.Second, "5m30s"},
		{0, "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, fmtDur(tt.dur))
		})
	}
}
