package analyze

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/model"
)

// tiedDetails builds two workflows whose jobs, steps and failures tie on
// every value an analyzer sorts by, so only a tie-break can order them. Every
// job slows down at the same run and has one slow run, so each analyzer that
// walks a map of jobs has several findings to order.
func tiedDetails() (all, successful []model.RunDetail) {
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	for wf := int64(1); wf <= 2; wf++ {
		for i := range 40 {
			start := base.Add(time.Duration(i) * time.Hour)
			jobDuration := 4 * time.Minute
			switch {
			case i == 7:
				jobDuration = 40 * time.Minute
			case i >= 20:
				jobDuration = 6 * time.Minute
			}
			conclusion := "success"
			if i%5 == 0 {
				conclusion = "failure"
			}
			var jobs []model.Job
			for j, name := range []string{"build", "lint", "test", "docs"} {
				// "docs" waits for the other three, which run side by side.
				jobStart := start
				if name == "docs" {
					jobStart = start.Add(jobDuration)
				}
				job := model.Job{
					ID: wf*10000 + int64(i)*10 + int64(j), Name: name, Conclusion: conclusion,
					StartedAt: jobStart, CompletedAt: jobStart.Add(jobDuration), Labels: []string{"ubuntu-latest"},
				}
				for s, stepName := range []string{"Checkout", "Install", "Compile", "Verify"} {
					stepStart := jobStart.Add(time.Duration(s) * time.Minute)
					step := model.Step{Name: stepName, Conclusion: "success", StartedAt: stepStart, CompletedAt: stepStart.Add(time.Minute)}
					// Each failed run fails a different job at a different step: equal counts.
					if conclusion == "failure" && j == (i/5)%4 && s == (i/5)%4 {
						step.Conclusion = "failure"
					}
					job.Steps = append(job.Steps, step)
				}
				jobs = append(jobs, job)
			}
			d := model.RunDetail{
				Run: model.WorkflowRun{
					ID: wf*1000 + int64(i), WorkflowID: wf, Status: "completed", Conclusion: conclusion,
					HeadSHA: fmt.Sprintf("%040d", i), RunAttempt: 1,
					CreatedAt: start, StartedAt: start, UpdatedAt: start.Add(2 * jobDuration),
				},
				Jobs: jobs,
			}
			all = append(all, d)
			if conclusion == "success" {
				successful = append(successful, d)
			}
		}
	}
	return all, successful
}

func TestEngine_OutputIsIdenticalAcrossRunsWhenValuesTie(t *testing.T) {
	all, successful := tiedDetails()
	names := map[int64]string{1: "Alpha", 2: "Beta"}

	var first []json.RawMessage
	for run := range 10 {
		result := NewEngine(DefaultAnalyzers()...).Run(t.Context(), successful, all, nil, names)
		require.Empty(t, result.Diagnostics)
		if run == 0 {
			types := make(map[string]int)
			for _, f := range result.Findings {
				types[f.Type]++
			}
			for _, analyzer := range DefaultAnalyzers() {
				if analyzer.Name() == TypeRunner {
					continue // nothing to say about right-sized runners
				}
				require.NotZero(t, types[analyzer.Name()], "the fixture gives %s nothing to order", analyzer.Name())
			}
		}

		got := make([]json.RawMessage, 0, len(result.Findings)+2)
		for _, f := range result.Findings {
			raw, err := json.Marshal(f)
			require.NoError(t, err)
			got = append(got, raw)
		}
		for _, extra := range []any{result.Meta, result.Reliability} {
			raw, err := json.Marshal(extra)
			require.NoError(t, err)
			got = append(got, raw)
		}
		if run == 0 {
			first = got
			continue
		}
		require.Len(t, got, len(first))
		for i := range got {
			require.Equal(t, string(first[i]), string(got[i]), "run %d differs from run 0 at item %d", run, i)
		}
	}
}
