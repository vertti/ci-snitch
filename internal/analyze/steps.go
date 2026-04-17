package analyze

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// StepTimingDetail contains step-level timing for a job.
type StepTimingDetail struct {
	WorkflowName string        `json:"workflow_name"`
	JobName      string        `json:"job_name"`
	TotalRuns    int           `json:"total_runs"`
	Steps        []StepSummary `json:"steps"`
}

// StepSummary holds timing stats for a single step.
type StepSummary struct {
	Name       string   `json:"name"`
	Runs       int      `json:"runs"`
	Median     Duration `json:"median"`
	P95        Duration `json:"p95"`
	PctOfJob   float64  `json:"pct_of_job"`
	Volatility float64  `json:"volatility"`
}

// DetailType implements FindingDetail.
func (StepTimingDetail) DetailType() string { return TypeSteps }

// StepAnalyzer identifies the slowest and most variable steps per job.
type StepAnalyzer struct {
	// TopN is the max number of steps to report per job (default: 3).
	TopN int
}

// Name implements Analyzer.
func (StepAnalyzer) Name() string { return "steps" }

const (
	minRunsForSteps   = 3
	defaultTopNSteps  = 3
	minStepDurationMs = 500 // ignore steps shorter than 500ms median
)

type stepAccum struct {
	durations []time.Duration
}

// Analyze implements Analyzer.
func (s StepAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) == 0 {
		return nil, nil
	}

	topN := s.TopN
	if topN == 0 {
		topN = defaultTopNSteps
	}

	type jobKey struct {
		wfID int64
		job  string
	}

	// Collect step durations per (workflow, job, step)
	jobSteps := make(map[jobKey]map[string]*stepAccum)
	jobMedians := make(map[jobKey]time.Duration) // for pct-of-job calculation
	jobRuns := make(map[jobKey]int)

	for _, d := range ac.Details {
		wfID := d.Run.WorkflowID
		for _, j := range d.Jobs {
			jk := jobKey{wfID, j.Name}
			jobRuns[jk]++

			if jobSteps[jk] == nil {
				jobSteps[jk] = make(map[string]*stepAccum)
			}
			for _, st := range j.Steps {
				dur := st.Duration()
				if dur <= 0 {
					continue
				}
				sa := jobSteps[jk][st.Name]
				if sa == nil {
					sa = &stepAccum{}
					jobSteps[jk][st.Name] = sa
				}
				sa.durations = append(sa.durations, dur)
			}
		}
	}

	// Collect job-level durations and compute medians
	jobDurs := make(map[jobKey][]time.Duration)
	for _, d := range ac.Details {
		for _, j := range d.Jobs {
			dur := j.Duration()
			if dur > 0 {
				jk := jobKey{d.Run.WorkflowID, j.Name}
				jobDurs[jk] = append(jobDurs[jk], dur)
			}
		}
	}
	for jk, durs := range jobDurs {
		jobMedians[jk] = percentile(durs, 50)
	}

	var findings []Finding
	for jk, steps := range jobSteps {
		if jobRuns[jk] < minRunsForSteps {
			continue
		}
		wfName := ac.WorkflowName(jk.wfID)
		jobMed := jobMedians[jk]

		summaries := summarizeSteps(steps, jobMed)

		if len(summaries) == 0 {
			continue
		}

		// Sort by median descending (slowest first)
		slices.SortFunc(summaries, func(a, b StepSummary) int {
			return cmp.Compare(b.Median, a.Median)
		})

		// Keep top N
		if len(summaries) > topN {
			summaries = summaries[:topN]
		}

		findings = append(findings, Finding{
			Type:     TypeSteps,
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("Slowest steps in %s / %s", wfName, jk.job),
			Description: fmt.Sprintf("Top %d steps by duration (of %d runs)",
				len(summaries), jobRuns[jk]),
			Detail: StepTimingDetail{
				WorkflowName: wfName,
				JobName:      jk.job,
				TotalRuns:    jobRuns[jk],
				Steps:        summaries,
			},
		})
	}

	// Precompute median by job name for O(1) sort comparisons
	medianByJob := make(map[string]time.Duration)
	for jk, med := range jobMedians {
		medianByJob[jk.job] = med
	}

	// Sort findings by job median descending (slowest jobs first)
	slices.SortFunc(findings, func(a, b Finding) int {
		ad, _ := a.Detail.(StepTimingDetail)
		bd, _ := b.Detail.(StepTimingDetail)
		return cmp.Compare(medianByJob[bd.JobName], medianByJob[ad.JobName])
	})

	return findings, nil
}

func summarizeSteps(steps map[string]*stepAccum, jobMed time.Duration) []StepSummary {
	var summaries []StepSummary
	for name, sa := range steps {
		if len(sa.durations) < minRunsForSteps {
			continue
		}
		slices.Sort(sa.durations)
		med := percentile(sa.durations, 50)
		if med.Milliseconds() < minStepDurationMs {
			continue
		}
		p95 := percentile(sa.durations, 95)

		pctOfJob := 0.0
		if jobMed > 0 {
			pctOfJob = float64(med) / float64(jobMed) * 100
		}
		vol := 0.0
		if med > 0 {
			vol = float64(p95) / float64(med)
		}

		summaries = append(summaries, StepSummary{
			Name:       name,
			Runs:       len(sa.durations),
			Median:     Duration(med),
			P95:        Duration(p95),
			PctOfJob:   pctOfJob,
			Volatility: vol,
		})
	}
	return summaries
}
