package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/preprocess"
)

// TestIntegration_RealWorldPatterns exercises the full analysis pipeline against
// anonymized data patterns observed in a production monorepo (2000+ runs, 30 days).
// Each sub-test targets a specific correctness concern found during manual verification.

// makeRun is a test helper that creates a WorkflowRun with common defaults.
func makeRun(id int64, wfID int64, conclusion string, created time.Time, dur time.Duration) model.WorkflowRun {
	return model.WorkflowRun{
		ID:         id,
		WorkflowID: wfID,
		Status:     "completed",
		Conclusion: conclusion,
		HeadSHA:    "deadbeef12345678",
		CreatedAt:  created,
		StartedAt:  created,
		UpdatedAt:  created.Add(dur),
	}
}

// TestIntegration_FailureRateWithCancelledAndDuplicates verifies that the failure
// analyzer produces correct rates when the input contains:
// - cancelled runs (should not count as failures)
// - duplicate run IDs (from API date window overlap — should be deduped)
// - retried runs (multiple attempts for same run ID)
//
// Real-world pattern: "tests" workflow had 619 unique runs in DB but the tool
// reported 704 due to un-deduped AllDetails. Failure rate was 25% (176/704)
// instead of the correct 23% (142/619).
func TestIntegration_FailureRateWithCancelledAndDuplicates(t *testing.T) {
	base := time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC)
	const wfID = 100

	// Build realistic data: 60 success, 15 failure, 10 cancelled = 85 unique runs
	var allDetails []model.RunDetail
	runID := int64(1000)

	for i := range 60 {
		start := base.Add(time.Duration(i) * time.Hour)
		allDetails = append(allDetails, model.RunDetail{
			Run: makeRun(runID, wfID, "success", start, 8*time.Minute),
		})
		runID++
	}
	for i := range 15 {
		start := base.Add(time.Duration(60+i) * time.Hour)
		allDetails = append(allDetails, model.RunDetail{
			Run: makeRun(runID, wfID, "failure", start, 7*time.Minute),
			Jobs: []model.Job{{
				Name: "build", Status: "completed", Conclusion: "failure",
				Steps: []model.Step{
					{Name: "Checkout", Status: "completed", Conclusion: "success"},
					{Name: "Run tests", Status: "completed", Conclusion: "failure"},
				},
			}},
		})
		runID++
	}
	for i := range 10 {
		start := base.Add(time.Duration(75+i) * time.Hour)
		allDetails = append(allDetails, model.RunDetail{
			Run: makeRun(runID, wfID, "cancelled", start, 3*time.Minute),
		})
		runID++
	}

	// Add duplicates simulating API date window overlap (same IDs, same data)
	allDetails = append(allDetails, allDetails[:10]...)

	// Add retries: 3 runs that failed then succeeded (same ID, different attempt)
	for i := range 3 {
		failedAttempt := allDetails[60+i] // one of the failures
		retryRun := failedAttempt
		retryRun.Run.RunAttempt = 2
		retryRun.Run.Conclusion = "success"
		allDetails = append(allDetails, retryRun)
	}

	// Dedup (this is what the fix does in analyze.go)
	deduped := preprocess.DeduplicateRetries(allDetails)

	// Verify dedup removed duplicates and kept latest attempts
	assert.Len(t, deduped, 85, "should have 85 unique runs after dedup")

	// Run the failure analyzer
	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		AllDetails:    deduped,
		WorkflowNames: map[int64]string{wfID: "tests"},
	})
	require.NoError(t, err)
	require.Len(t, findings, 1)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)

	// 3 retried failures became successes, so: 12 failures, 10 cancelled, 63 success = 85 total
	assert.Equal(t, 85, d.TotalRuns, "total should match unique run count")
	assert.Equal(t, 12, d.FailureCount, "failures should exclude cancelled and count retried-as-success correctly")
	assert.Equal(t, 10, d.CancellationCount, "cancelled count should be exact")
	assert.InDelta(t, 12.0/85.0, d.FailureRate, 0.001, "failure rate should use deduped totals")
	assert.InDelta(t, 10.0/85.0, d.CancellationRate, 0.001)
}

// TestIntegration_FailingStepConsistency verifies that when all failures in a
// workflow hit the same step, the failing step attribution correctly reports it.
//
// Real-world pattern: "Claude Code Review" had 153 failures, ALL at step
// "Run Code Review with Claude" — a single root cause, not random flakiness.
func TestIntegration_FailingStepConsistency(t *testing.T) {
	base := time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC)
	const wfID = 200

	var allDetails []model.RunDetail
	runID := int64(2000)

	// 30 success + 20 failure (all same step)
	for i := range 30 {
		start := base.Add(time.Duration(i) * time.Hour)
		allDetails = append(allDetails, model.RunDetail{
			Run: makeRun(runID, wfID, "success", start, 2*time.Minute),
		})
		runID++
	}
	for i := range 20 {
		start := base.Add(time.Duration(30+i) * time.Hour)
		allDetails = append(allDetails, model.RunDetail{
			Run: makeRun(runID, wfID, "failure", start, 1*time.Minute),
			Jobs: []model.Job{{
				Name: "review", Status: "completed", Conclusion: "failure",
				Steps: []model.Step{
					{Name: "Checkout", Status: "completed", Conclusion: "success"},
					{Name: "Run Code Review", Status: "completed", Conclusion: "failure"},
				},
			}},
		})
		runID++
	}

	analyzer := FailureAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		AllDetails:    allDetails,
		WorkflowNames: map[int64]string{wfID: "Code Review"},
	})
	require.NoError(t, err)
	require.Len(t, findings, 1)

	d, ok := findings[0].Detail.(FailureDetail)
	require.True(t, ok)

	require.Len(t, d.FailingSteps, 1, "all failures hit same step")
	assert.Equal(t, "Run Code Review", d.FailingSteps[0].StepName)
	assert.Equal(t, 20, d.FailingSteps[0].Count, "all 20 failures at same step")
}

// TestIntegration_OutlierDoesNotCauseChangepoint verifies that a single extreme
// outlier in an otherwise stable job does NOT produce a regression changepoint.
//
// Real-world pattern: "Deploy to Test" had one 38-min run among 10-min runs.
// Before the outlier-clamping fix, this caused a false persistent regression.
func TestIntegration_OutlierDoesNotCauseChangepoint(t *testing.T) {
	base := time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC)
	const wfID = 300

	// 30 runs of a deploy job: ~600s ± jitter, with one 2400s outlier at position 20
	durations := []int{
		648, 638, 686, 669, 630, 645, 669, 644, 683, 687,
		672, 689, 675, 665, 648, 719, 737, 680, 657, 705,
		2367, // extreme outlier (CloudFormation rollback)
		675, 654, 673, 671, 675, 643, 647, 674, 631,
	}

	var details []model.RunDetail
	for i, dur := range durations {
		start := base.Add(time.Duration(i) * 2 * time.Hour)
		jobStart := start
		jobEnd := start.Add(time.Duration(dur) * time.Second)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(3000 + i), WorkflowID: wfID,
				Status: "completed", Conclusion: "success",
				HeadSHA: "abc12345", CreatedAt: start, StartedAt: start,
				UpdatedAt: jobEnd,
			},
			Jobs: []model.Job{{
				Name: "deploy", StartedAt: jobStart, CompletedAt: jobEnd,
			}},
		})
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:       details,
		WorkflowNames: map[int64]string{wfID: "Deploy"},
	})
	require.NoError(t, err)

	// Should not detect the outlier as a regression
	for _, f := range findings {
		d, ok := f.Detail.(ChangePointDetail)
		if !ok {
			continue
		}
		if d.Direction == DirectionSlowdown && d.PctChange > 20 {
			t.Errorf("single outlier should not produce a +%0.f%% regression finding", d.PctChange)
		}
	}
}

// TestIntegration_GenuineSpeedupDetected verifies that a real sustained speedup
// IS detected, even with outlier clamping enabled.
//
// Real-world pattern: "Deploy to Test" genuinely sped up from ~670s to ~590s
// in the last 2 weeks. The tool correctly detected this as a persistent speedup.
func TestIntegration_GenuineSpeedupDetected(t *testing.T) {
	base := time.Date(2026, 3, 16, 8, 0, 0, 0, time.UTC)
	const wfID = 400

	// 20 runs at ~670s, then 20 runs at ~590s (genuine speedup)
	var details []model.RunDetail
	dursBefore := []int{648, 686, 669, 645, 669, 644, 683, 687, 672, 689, 675, 665, 648, 719, 680, 657, 705, 675, 654, 673}
	dursAfter := []int{565, 555, 595, 577, 551, 567, 580, 566, 536, 557, 563, 577, 550, 536, 581, 580, 573, 541, 578, 587}

	all := make([]int, 0, len(dursBefore)+len(dursAfter))
	all = append(all, dursBefore...)
	all = append(all, dursAfter...)
	for i, dur := range all {
		start := base.Add(time.Duration(i) * 12 * time.Hour)
		jobEnd := start.Add(time.Duration(dur) * time.Second)
		details = append(details, model.RunDetail{
			Run: model.WorkflowRun{
				ID: int64(4000 + i), WorkflowID: wfID,
				Status: "completed", Conclusion: "success",
				HeadSHA: "def45678", CreatedAt: start, StartedAt: start,
				UpdatedAt: jobEnd,
			},
			Jobs: []model.Job{{
				Name: "deploy-infra", StartedAt: start, CompletedAt: jobEnd,
			}},
		})
	}

	analyzer := ChangePointAnalyzer{}
	findings, err := analyzer.Analyze(context.Background(), &AnalysisContext{
		Details:       details,
		WorkflowNames: map[int64]string{wfID: "Deploy"},
	})
	require.NoError(t, err)

	var speedup *ChangePointDetail
	for _, f := range findings {
		d, ok := f.Detail.(ChangePointDetail)
		if ok && d.Direction == DirectionSpeedup {
			speedup = &d
			break
		}
	}
	require.NotNil(t, speedup, "should detect genuine speedup")
	assert.Less(t, speedup.PctChange, -10.0, "speedup should be at least 10%")
	assert.Less(t, speedup.PValue, 0.05, "speedup should be statistically significant")
}

// TestIntegration_VolatilityLabel verifies volatility labels against manually
// computed values from real workflow data.
//
// Real-world data points:
//   - cleanup caches: vol=1.21 -> "stable"
//   - tests: vol=1.99 -> "variable"
//   - Deploy Test Env: vol=2.23 -> "spiky"
//   - Dependabot Updates: vol=4.05 -> "volatile"
func TestIntegration_VolatilityLabel(t *testing.T) {
	tests := []struct {
		name      string
		p95       time.Duration
		median    time.Duration
		wantLabel string
	}{
		{"stable (p95/med=1.21)", 17 * time.Second, 14 * time.Second, "stable"},
		{"variable (p95/med=1.99)", 1074 * time.Second, 541 * time.Second, "variable"},
		{"spiky (p95/med=2.23)", 3509 * time.Second, 1571 * time.Second, "spiky"},
		{"volatile (p95/med=4.05)", 271 * time.Second, 67 * time.Second, "volatile"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vol := float64(tt.p95) / float64(tt.median)
			label := volatilityLabel(vol)
			assert.Equal(t, tt.wantLabel, label)
		})
	}
}
