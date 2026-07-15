package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func TestParseCoreCount(t *testing.T) {
	tests := []struct {
		label string
		want  int
	}{
		// GitHub's split convention for larger runners — previously fell
		// through to the 2-core ubuntu default, producing "undersized —
		// consider larger runner" advice for a 16-core machine.
		{"ubuntu-latest-16-cores", 16},
		{"windows-latest-8-cores", 8},
		{"macos-latest-12-core", 12},
		// Adjacent conventions used by third-party runner vendors.
		{"blacksmith-16vcpu-ubuntu-2404", 16},
		{"ubuntu-22.04-32core", 32},
		{"namespace-profile-4cores", 4},
		// Standard hosted runners fall back to documented defaults.
		{"ubuntu-latest", 2},
		{"windows-latest", 2},
		{"macos-latest", 3}, // current arm64 macOS runners have 3 vCPUs
		// Unknown labels carry no core information.
		{"self-hosted", 0},
	}
	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			assert.Equal(t, tt.want, parseCoreCount(tt.label))
		})
	}
}

func TestRunnerAnalyzer_NoCostClaimWithoutKnownMultiplier(t *testing.T) {
	// A 16-vCPU third-party runner with a short job is legitimately flagged
	// oversized, but "save ~1x cost" invents a GitHub bill for a runner
	// GitHub doesn't bill (the 1x is the unknown-label default).
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	var details []model.RunDetail
	for i := range 6 {
		start := base.Add(time.Duration(i) * time.Hour)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(1000 + i), WorkflowID: 100, WorkflowName: "CI",
				Status: "completed", Conclusion: "success",
				CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(2 * time.Minute),
			},
			Jobs: []model.Job{{
				Name: "quick", StartedAt: start, CompletedAt: start.Add(30 * time.Second),
				Labels: []string{"blacksmith-16vcpu-ubuntu-2404"},
			}},
		})
	}

	analyzer := RunnerAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:       details,
		WorkflowNames: map[int64]string{100: "CI"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, findings, "a 30s job on 16 cores is still oversized")

	d, ok := findings[0].Detail.(RunnerDetail)
	require.True(t, ok)
	assert.Equal(t, "oversized", d.Issue)
	assert.NotContains(t, d.Suggestion, "cost",
		"no cost claim without a known GitHub billing multiplier")
}
