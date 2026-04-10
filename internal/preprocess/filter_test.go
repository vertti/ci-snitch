package preprocess

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func makeDetail(id int64, branch, conclusion string, attempt int) model.RunDetail {
	return model.RunDetail{
		Run: model.WorkflowRun{
			ID:         id,
			HeadBranch: branch,
			Conclusion: conclusion,
			Status:     "completed",
			RunAttempt: attempt,
		},
	}
}

func makeDetailWithJobs(id int64, jobNames ...string) model.RunDetail {
	d := makeDetail(id, "main", "success", 1)
	for _, name := range jobNames {
		d.Jobs = append(d.Jobs, model.Job{Name: name})
	}
	return d
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

func TestGroupMatrixJobs(t *testing.T) {
	details := []model.RunDetail{
		makeDetailWithJobs(1, "build", "test (ubuntu-latest, 20)", "test (macos-latest, 20)"),
		makeDetailWithJobs(2, "build", "test (ubuntu-latest, 20)", "test (macos-latest, 20)"),
	}

	groups := GroupMatrixJobs(details)
	assert.Contains(t, groups, "build")
	assert.Empty(t, groups["build"]) // no variants for "build"

	assert.Contains(t, groups, "test")
	assert.Len(t, groups["test"], 2)
	assert.ElementsMatch(t, []string{"ubuntu-latest, 20", "macos-latest, 20"}, groups["test"])
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

	// Should have: run 1 (attempt 2, deduped), run 4 (success on main)
	// Run 2 excluded (failure), run 3 excluded (wrong branch)
	assert.Len(t, result, 2)
	assert.Equal(t, int64(1), result[0].Run.ID)
	assert.Equal(t, 2, result[0].Run.RunAttempt)
	assert.Equal(t, int64(4), result[1].Run.ID)

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

func TestRun_AllFiltered(t *testing.T) {
	details := []model.RunDetail{
		makeDetail(1, "main", "failure", 1),
		makeDetail(2, "main", "cancelled", 1),
	}

	result, warnings := Run(details, Options{Branch: "main"})
	assert.Empty(t, result)
	assert.NotEmpty(t, warnings)
}
