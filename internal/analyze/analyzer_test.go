package analyze

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

func TestJobSeries_ChronologicalFilteredMemoized(t *testing.T) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	ac := &AnalysisContext{Details: []model.RunDetail{
		// Listed out of chronological order on purpose: the series must be
		// ordered by run CreatedAt, not Details order — CUSUM change points
		// are meaningless on a shuffled series.
		{
			Run: model.WorkflowRun{ID: 2, WorkflowID: 7, CreatedAt: base.Add(time.Hour)},
			Jobs: []model.Job{
				{Name: "build", StartedAt: base.Add(time.Hour), CompletedAt: base.Add(time.Hour + 2*time.Minute)},
			},
		},
		{
			Run: model.WorkflowRun{ID: 1, WorkflowID: 7, CreatedAt: base},
			Jobs: []model.Job{
				{Name: "build", StartedAt: base, CompletedAt: base.Add(time.Minute)},
				{Name: "never-ran"}, // zero duration: excluded
			},
		},
	}}

	series := ac.JobSeries()
	js := series[JobKey{WorkflowID: 7, Job: "build"}]
	require.NotNil(t, js)
	assert.Equal(t, []float64{60, 120}, js.Durations, "chronological despite listing order")
	require.Len(t, js.Refs, 2)
	assert.Equal(t, RunJobRef{DetailIdx: 1, JobIdx: 0}, js.Refs[0], "first sample is the earlier run")
	assert.Equal(t, RunJobRef{DetailIdx: 0, JobIdx: 0}, js.Refs[1])

	_, ok := series[JobKey{WorkflowID: 7, Job: "never-ran"}]
	assert.False(t, ok, "jobs with no positive duration produce no series")

	// Memoized: a second call returns the same map, not a rebuild.
	series[JobKey{WorkflowID: 99, Job: "sentinel"}] = &JobSeries{}
	_, ok = ac.JobSeries()[JobKey{WorkflowID: 99, Job: "sentinel"}]
	assert.True(t, ok, "JobSeries must be computed once and shared")
}
