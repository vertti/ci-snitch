package analyze

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostProcess_CategorizeOscillating(t *testing.T) {
	findings := []Finding{
		{Type: TypeChangepoint, Severity: SeverityWarning, Detail: ChangePointDetail{JobName: "test", Direction: DirectionSlowdown, Date: time.Now().Add(-3 * time.Hour)}},
		{Type: TypeChangepoint, Severity: SeverityWarning, Detail: ChangePointDetail{JobName: "test", Direction: DirectionSpeedup, Date: time.Now().Add(-2 * time.Hour)}},
		{Type: TypeChangepoint, Severity: SeverityWarning, Detail: ChangePointDetail{JobName: "test", Direction: DirectionSlowdown, Date: time.Now().Add(-1 * time.Hour)}},
	}

	result := postProcess(findings)
	for _, f := range result {
		d, ok := f.Detail.(ChangePointDetail)
		if !ok {
			continue
		}
		assert.Equal(t, CategoryOscillating, d.Category, "3+ shifts should be oscillating")
	}
}

func TestPostProcess_DedupRegressions(t *testing.T) {
	findings := []Finding{
		{Type: TypeChangepoint, Severity: SeverityWarning, Detail: ChangePointDetail{
			JobName: "build", Direction: DirectionSlowdown, Persistence: PersistencePersistent,
			Date: time.Now().Add(-2 * time.Hour), PctChange: 15,
		}},
		{Type: TypeChangepoint, Severity: SeverityWarning, Detail: ChangePointDetail{
			JobName: "build", Direction: DirectionSlowdown, Persistence: PersistencePersistent,
			Date: time.Now().Add(-1 * time.Hour), PctChange: 20,
		}},
	}

	result := postProcess(findings)
	var regressions []ChangePointDetail
	for _, f := range result {
		d, ok := f.Detail.(ChangePointDetail)
		if ok && d.Category == CategoryRegression {
			regressions = append(regressions, d)
		}
	}
	require.Len(t, regressions, 1, "should keep only latest regression per job")
	assert.InDelta(t, 20, regressions[0].PctChange, 0.1, "should keep the latest one")
}

func TestPostProcess_GroupOutliers(t *testing.T) {
	findings := []Finding{
		{Type: TypeOutlier, Severity: SeverityWarning, Detail: OutlierDetail{WorkflowName: "CI", JobName: "test", Duration: Duration(5 * time.Minute), Percentile: 96, CommitSHA: "aaa"}},
		{Type: TypeOutlier, Severity: SeverityCritical, Detail: OutlierDetail{WorkflowName: "CI", JobName: "test", Duration: Duration(10 * time.Minute), Percentile: 99, CommitSHA: "bbb"}},
		{Type: TypeOutlier, Severity: SeverityWarning, Detail: OutlierDetail{WorkflowName: "CI", JobName: "test", Duration: Duration(6 * time.Minute), Percentile: 97, CommitSHA: "ccc"}},
	}

	result := postProcess(findings)
	var groups []OutlierGroupDetail
	for _, f := range result {
		if d, ok := f.Detail.(OutlierGroupDetail); ok {
			groups = append(groups, d)
		}
	}
	require.Len(t, groups, 1, "3 outliers for same job should become 1 group")
	assert.Equal(t, 3, groups[0].Count)
	assert.Equal(t, Duration(10*time.Minute), groups[0].WorstDuration)
	assert.Equal(t, SeverityCritical, groups[0].MaxSeverity)
}

func TestPostProcess_HighOverlapDowngradedToMinor(t *testing.T) {
	// A changepoint where >50% of after-points are within the before IQR
	// should be downgraded to minor (likely outlier-driven, not a real shift)
	findings := []Finding{
		{Type: TypeChangepoint, Severity: SeverityWarning, Detail: ChangePointDetail{
			JobName: "deploy", Direction: DirectionSlowdown,
			Persistence: PersistencePersistent, PctChange: 30,
			OverlapRatio: 0.7, // 70% overlap — not a real shift
		}},
	}

	result := postProcess(findings)
	var cps []ChangePointDetail
	for _, f := range result {
		if d, ok := f.Detail.(ChangePointDetail); ok {
			cps = append(cps, d)
		}
	}
	require.Len(t, cps, 1)
	assert.Equal(t, CategoryMinor, cps[0].Category, "high overlap should be downgraded to minor")
}

func TestPostProcess_LowOverlapKeepsRegression(t *testing.T) {
	findings := []Finding{
		{Type: TypeChangepoint, Severity: SeverityWarning, Detail: ChangePointDetail{
			JobName: "build", Direction: DirectionSlowdown,
			Persistence: PersistencePersistent, PctChange: 25,
			OverlapRatio: 0.1, // 10% overlap — genuine shift
		}},
	}

	result := postProcess(findings)
	var cps []ChangePointDetail
	for _, f := range result {
		if d, ok := f.Detail.(ChangePointDetail); ok {
			cps = append(cps, d)
		}
	}
	require.Len(t, cps, 1)
	assert.Equal(t, CategoryRegression, cps[0].Category, "low overlap should remain regression")
}

func TestPostProcess_FilterLowFailureRate(t *testing.T) {
	findings := []Finding{
		{Type: TypeFailure, Severity: SeverityInfo, Detail: FailureDetail{Workflow: "cleanup", FailureRate: 0.01}},
		{Type: TypeFailure, Severity: SeverityWarning, Detail: FailureDetail{Workflow: "tests", FailureRate: 0.10}},
	}

	result := postProcess(findings)
	var failures []FailureDetail
	for _, f := range result {
		if d, ok := f.Detail.(FailureDetail); ok {
			failures = append(failures, d)
		}
	}
	require.Len(t, failures, 1, "sub-5% failure rate should be filtered")
	assert.Equal(t, "tests", failures[0].Workflow)
}
