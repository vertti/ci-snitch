package preprocess

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func makeDetail(id int64, branch, conclusion string, attempt int) model.RunDetail {
	return model.RunDetail{
		Run: model.WorkflowRun{
			ID:           id,
			WorkflowName: "CI",
			HeadBranch:   branch,
			Conclusion:   conclusion,
			Status:       "completed",
			RunAttempt:   attempt,
		},
	}
}

func TestFilterByBranch(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "success", 1),
		makeDetail(2, "feature", "success", 1),
		makeDetail(3, "main", "success", 1),
		makeDetail(4, "develop", "success", 1),
	}

	result := FilterByBranch(details, "main")
	assert.Len(t, result, 2)
	assert.Equal(t, int64(1), result[0].Run.ID)
	assert.Equal(t, int64(3), result[1].Run.ID)
}

func TestFilterByBranch_NoMatch(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "success", 1),
	}
	result := FilterByBranch(details, "nonexistent")
	assert.Empty(t, result)
}

func TestExcludeFailures(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "success", 1),
		makeDetail(2, "main", "failure", 1),
		makeDetail(3, "main", "cancelled", 1),
		makeDetail(4, "main", "success", 1),
	}

	result := ExcludeFailures(details)
	assert.Len(t, result, 2)
	assert.Equal(t, int64(1), result[0].Run.ID)
	assert.Equal(t, int64(4), result[1].Run.ID)
}

func TestRun_ExcludesRerunAttemptsFromDurationSeries(t *testing.T) {
	// Dedup keeps the latest attempt; for "re-run failed jobs" that
	// attempt's wall clock covers only the re-run subset — a 40-minute
	// workflow can contribute a 6-minute "duration" to the summary/
	// changepoint/outlier series. Re-run attempts stay in AllDetails (for
	// failure/rerun/cost analysis) but leave the duration series.
	details := []model.RunDetail{
		makeDetail(1, "main", "success", 1),
		makeDetail(2, "main", "failure", 1),
		makeDetail(2, "main", "success", 2), // partial re-run: wall clock not comparable
		makeDetail(3, "main", "success", 1),
	}

	result, warnings := Run(details, Options{})
	ids := make([]int64, 0, len(result))
	for _, d := range result {
		ids = append(ids, d.Run.ID)
	}
	assert.Equal(t, []int64{1, 3}, ids,
		"the attempt-2 run must not contribute a wall-clock sample")

	found := false
	for _, w := range warnings {
		if strings.Contains(w.Message, "re-run attempt") {
			found = true
		}
	}
	assert.True(t, found, "exclusion must be visible in the diagnostics: %v", warnings)
}

func TestDeduplicateRetries(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "failure", 1),
		makeDetail(1, "main", "success", 2), // retry of run 1
		makeDetail(2, "main", "success", 1),
	}

	result := DeduplicateRetries(details)
	require.Len(t, result, 2)
	// Run 1 should keep attempt 2
	assert.Equal(t, int64(1), result[0].Run.ID)
	assert.Equal(t, 2, result[0].Run.RunAttempt)
	assert.Equal(t, "success", result[0].Run.Conclusion)
	// Run 2 unchanged
	assert.Equal(t, int64(2), result[1].Run.ID)
}

func TestDeduplicateRetries_PreservesOrder(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(3, "main", "success", 1),
		makeDetail(1, "main", "failure", 1),
		makeDetail(2, "main", "success", 1),
		makeDetail(1, "main", "success", 2),
	}

	result := DeduplicateRetries(details)
	require.Len(t, result, 3)
	assert.Equal(t, int64(3), result[0].Run.ID)
	assert.Equal(t, int64(1), result[1].Run.ID)
	assert.Equal(t, int64(2), result[2].Run.ID)
}

func TestParseMatrixJobName(t *testing.T) {
	tests := []struct {
		name        string
		wantBase    string
		wantVariant string
	}{
		{"build", "build", ""},
		{"test (ubuntu-latest, 20)", "test", "ubuntu-latest, 20"},
		{"deploy (production)", "deploy", "production"},
		{"lint", "lint", ""},
		{"build (macos-latest)", "build", "macos-latest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, variant := ParseMatrixJobName(tt.name)
			assert.Equal(t, tt.wantBase, base)
			assert.Equal(t, tt.wantVariant, variant)
		})
	}
}

func TestRun_FullPipeline(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "success", 1),
		makeDetail(1, "main", "success", 2), // retry
		makeDetail(2, "main", "failure", 1),
		makeDetail(3, "feature", "success", 1),
		makeDetail(4, "main", "success", 1),
	}

	result, warnings := Run(details, Options{Branch: "main"})

	// Should have: run 4 only. Run 1 dedups to attempt 2, whose wall clock
	// is not a comparable duration sample (F9); run 2 excluded (failure);
	// run 3 excluded (wrong branch).
	assert.Len(t, result, 1)
	assert.Equal(t, int64(4), result[0].Run.ID)

	assert.NotEmpty(t, warnings)
}

func TestRun_IncludeFailures(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "success", 1),
		makeDetail(2, "main", "failure", 1),
	}

	result, _ := Run(details, Options{Branch: "main", IncludeFailures: true})
	assert.Len(t, result, 2)
}

func TestRun_NoBranchFilter(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "success", 1),
		makeDetail(2, "feature", "success", 1),
	}

	result, _ := Run(details, Options{})
	assert.Len(t, result, 2)
}

func TestComputeRerunStats(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "failure", 1), // run 1, attempt 1
		makeDetail(1, "main", "failure", 2), // run 1, attempt 2
		makeDetail(1, "main", "success", 3), // run 1, attempt 3 (finally passed)
		makeDetail(2, "main", "success", 1), // run 2, no retries
		makeDetail(3, "main", "failure", 1), // run 3, attempt 1
		makeDetail(3, "main", "success", 2), // run 3, attempt 2
	}

	stats := ComputeRerunStats(details)
	require.Contains(t, stats, int64(0))

	ci := stats[int64(0)]
	assert.Equal(t, 3, ci.UniqueRuns)
	assert.Equal(t, 2, ci.RetriedRuns)   // runs 1 and 3 had retries
	assert.Equal(t, 3, ci.ExtraAttempts) // run 1 had 2 extra, run 3 had 1 extra
	assert.InDelta(t, 2.0/3.0, ci.RerunRate, 0.01)
}

func TestComputeRerunStats_NoRetries(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "success", 1),
		makeDetail(2, "main", "success", 1),
	}

	stats := ComputeRerunStats(details)
	assert.Empty(t, stats, "should not report workflows with no retries")
}

func TestRun_AllFiltered(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "failure", 1),
		makeDetail(2, "main", "cancelled", 1),
	}

	result, warnings := Run(details, Options{Branch: "main"})
	assert.Empty(t, result)
	assert.NotEmpty(t, warnings)
}
