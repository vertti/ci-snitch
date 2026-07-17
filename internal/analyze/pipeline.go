package analyze

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vertti/ci-snitch/internal/model"
)

// TypePipeline is the finding type for pipeline analysis.
const TypePipeline = "pipeline"

// PipelineDetail contains pipeline structure analysis for a workflow.
type PipelineDetail struct {
	Workflow        string          `json:"workflow"`
	TotalRuns       int             `json:"total_runs"`
	MedianWallClock Duration        `json:"median_wall_clock"`
	MedianJobSum    Duration        `json:"median_job_sum"`
	Parallelism     float64         `json:"parallelism"` // 0-1: fraction of job time that runs in parallel
	Stages          []PipelineStage `json:"stages"`
	CriticalPath    string          `json:"critical_path"` // name of the slowest stage
}

// PipelineStage represents a group of jobs that run concurrently.
type PipelineStage struct {
	Name          string   `json:"name"`
	Jobs          []string `json:"jobs"`
	Duration      Duration `json:"duration"` // median wall-clock duration of this stage
	PctOfPipeline float64  `json:"pct_of_pipeline"`
	Sequential    bool     `json:"sequential"` // true if this stage waits for the previous to finish
	// PotentialSavings is the wall-clock upper bound of running this stage in
	// parallel with the previous one: min(prev duration, this duration).
	// An estimate, not a recommendation — job dependencies must be verified.
	PotentialSavings Duration `json:"potential_savings,omitempty"`
}

// DetailType implements FindingDetail.
func (PipelineDetail) DetailType() string { return TypePipeline }

// PipelineAnalyzer detects sequential dependency chains and parallelism efficiency.
type PipelineAnalyzer struct{}

// Name implements Analyzer.
func (PipelineAnalyzer) Name() string { return TypePipeline }

const minRunsForPipeline = 5

// Analyze implements Analyzer.
func (PipelineAnalyzer) Analyze(_ context.Context, ac *AnalysisContext) ([]Finding, error) {
	if len(ac.Details) == 0 {
		return nil, nil
	}

	// Group runs by workflow
	byWorkflow := make(map[int64][]model.RunDetail)
	for i := range ac.Details {
		wfID := ac.Details[i].Run.WorkflowID
		byWorkflow[wfID] = append(byWorkflow[wfID], ac.Details[i])
	}

	var findings []Finding
	for wfID, details := range byWorkflow {
		if len(details) < minRunsForPipeline {
			continue
		}
		// Check if this workflow has enough jobs to have pipeline structure
		maxJobs := 0
		for i := range details {
			if len(details[i].Jobs) > maxJobs {
				maxJobs = len(details[i].Jobs)
			}
		}
		if maxJobs < 2 {
			continue
		}

		wfName := ac.WorkflowName(wfID)
		finding := analyzePipeline(wfName, details)
		if finding != nil {
			findings = append(findings, *finding)
		}
	}

	// Sort by wall-clock time descending (slowest pipelines first)
	slices.SortFunc(findings, func(a, b Finding) int {
		ad, _ := a.Detail.(PipelineDetail)
		bd, _ := b.Detail.(PipelineDetail)
		return cmp.Compare(bd.MedianWallClock, ad.MedianWallClock)
	})

	return findings, nil
}

func analyzePipeline(wfName string, details []model.RunDetail) *Finding {
	// Compute per-run metrics
	var wallClocks, jobSums []time.Duration

	// Aggregate stage structure across runs
	type stageKey string
	stageTimings := make(map[stageKey][]time.Duration)
	stageJobs := make(map[stageKey]map[string]bool)

	for i := range details {
		wc := details[i].Duration()
		if wc <= 0 {
			continue
		}
		var js time.Duration
		for j := range details[i].Jobs {
			js += details[i].Jobs[j].Duration()
		}
		if js <= 0 {
			continue
		}
		wallClocks = append(wallClocks, wc)
		jobSums = append(jobSums, js)

		// Detect stages for this run
		stages := detectStages(details[i].Jobs)
		for _, stage := range stages {
			key := stageKey(stage.name)
			stageTimings[key] = append(stageTimings[key], stage.duration)
			if stageJobs[key] == nil {
				stageJobs[key] = make(map[string]bool)
			}
			for _, j := range stage.jobs {
				stageJobs[key][j] = true
			}
		}
	}

	if len(wallClocks) < minRunsForPipeline {
		return nil
	}

	slices.Sort(wallClocks)
	slices.Sort(jobSums)
	medWC := percentile(wallClocks, 50)
	medJS := percentile(jobSums, 50)

	parallelism := 0.0
	if medJS > 0 {
		parallelism = 1 - float64(medWC)/float64(medJS)
		if parallelism < 0 {
			parallelism = 0
		}
	}

	// Build ordered stage list from the most common run pattern
	// Use the run with the most jobs as the representative
	var bestRun model.RunDetail
	for i := range details {
		if len(details[i].Jobs) > len(bestRun.Jobs) {
			bestRun = details[i]
		}
	}
	repStages := detectStages(bestRun.Jobs)

	stageOrder := make([]stageKey, 0, len(repStages))
	for _, s := range repStages {
		stageOrder = append(stageOrder, stageKey(s.name))
	}

	var stages []PipelineStage
	var criticalPath string
	var maxStageDur time.Duration

	for i, key := range stageOrder {
		timings := stageTimings[key]
		if len(timings) == 0 {
			continue
		}
		slices.Sort(timings)
		medDur := percentile(timings, 50)

		jobs := make([]string, 0, len(stageJobs[key]))
		for j := range stageJobs[key] {
			jobs = append(jobs, j)
		}
		slices.Sort(jobs)

		pctOfPipeline := 0.0
		if medWC > 0 {
			pctOfPipeline = float64(medDur) / float64(medWC) * 100
		}

		stage := PipelineStage{
			Name:          string(key),
			Jobs:          jobs,
			Duration:      Duration(medDur),
			PctOfPipeline: pctOfPipeline,
			Sequential:    i > 0, // first stage is not sequential by definition
		}
		stages = append(stages, stage)

		if medDur > maxStageDur {
			maxStageDur = medDur
			criticalPath = string(key)
		}
	}

	applyPotentialSavings(stages)

	if len(stages) < 2 {
		return nil
	}

	detail := PipelineDetail{
		Workflow:        wfName,
		TotalRuns:       len(wallClocks),
		MedianWallClock: Duration(medWC),
		MedianJobSum:    Duration(medJS),
		Parallelism:     parallelism,
		Stages:          stages,
		CriticalPath:    criticalPath,
	}

	desc := fmt.Sprintf("%d%% parallel efficiency, %d stages, critical path: %s (%s)",
		int(parallelism*100), len(stages), criticalPath,
		Duration(maxStageDur).Round(time.Second))

	return &Finding{
		Type:        TypePipeline,
		Severity:    SeverityInfo,
		Title:       "Pipeline structure: " + wfName,
		Description: desc,
		Detail:      detail,
	}
}

// rawStage is an intermediate stage detected from a single run.
type rawStage struct {
	name     string
	jobs     []string
	start    time.Time
	end      time.Time
	duration time.Duration
}

// detectStages groups jobs into concurrent stages based on temporal overlap:
// a job that starts while the current stage is still running belongs to it,
// however staggered its start; a job that starts after the stage ended opens
// a new sequential stage.
func detectStages(jobs []model.Job) []rawStage {
	type timedJob struct {
		name  string
		start time.Time
		end   time.Time
	}

	var timed []timedJob
	for i := range jobs {
		if jobs[i].StartedAt.IsZero() || jobs[i].CompletedAt.IsZero() {
			continue
		}
		if jobs[i].Duration() <= 0 {
			continue
		}
		timed = append(timed, timedJob{jobs[i].Name, jobs[i].StartedAt, jobs[i].CompletedAt})
	}

	if len(timed) == 0 {
		return nil
	}

	// Sort by start time
	slices.SortFunc(timed, func(a, b timedJob) int {
		return a.start.Compare(b.start)
	})

	// Group into stages by temporal overlap: a job that starts while the
	// current stage is still running is concurrent with it, regardless of
	// start stagger (matrix fan-outs on constrained runner pools stagger
	// starts by minutes). Conversely, a job that starts after the stage
	// ended waited for it — a new, genuinely sequential stage. This also
	// makes the aggregated Sequential flag correct by construction: a stage
	// break can only happen once the previous stage has finished.
	var stages []rawStage
	current := rawStage{
		jobs:  []string{timed[0].name},
		start: timed[0].start,
		end:   timed[0].end,
	}

	for _, j := range timed[1:] {
		if j.start.Before(current.end) {
			// Same stage
			current.jobs = append(current.jobs, j.name)
			if j.end.After(current.end) {
				current.end = j.end
			}
		} else {
			// New stage
			current.duration = current.end.Sub(current.start)
			current.name = stageName(current.jobs)
			stages = append(stages, current)
			current = rawStage{
				jobs:  []string{j.name},
				start: j.start,
				end:   j.end,
			}
		}
	}
	// Close last stage
	current.duration = current.end.Sub(current.start)
	current.name = stageName(current.jobs)
	stages = append(stages, current)

	return stages
}

// applyPotentialSavings sets each sequential transition's parallelization
// upper bound: wall clock would drop from dur(prev)+dur(cur) to the max of
// the two if they ran in parallel.
func applyPotentialSavings(stages []PipelineStage) {
	for i := 1; i < len(stages); i++ {
		stages[i].PotentialSavings = min(stages[i-1].Duration, stages[i].Duration)
	}
}

// stageName creates a human-readable name for a stage based on its jobs.
// For reusable-workflow jobs (named "parent / child"), uses just the child
// name to avoid verbose doubled paths.
func stageName(jobs []string) string {
	if len(jobs) == 1 {
		return jobs[0]
	}
	if prefix := commonPrefix(jobs); prefix != "" {
		return fmt.Sprintf("%s (%d variants)", prefix, len(jobs))
	}
	short := make([]string, len(jobs))
	for i, j := range jobs {
		short[i] = lastPathSegment(j)
	}
	if len(short) <= 3 {
		return strings.Join(short, ", ")
	}
	return fmt.Sprintf("%s, +%d more", strings.Join(short[:2], ", "), len(short)-2)
}

// lastPathSegment returns the substring after the last " / " in a job name.
// Reusable workflow jobs are named like "tests / Merge artifacts"; for inline
// display we only need "Merge artifacts".
func lastPathSegment(name string) string {
	if i := strings.LastIndex(name, " / "); i >= 0 {
		return name[i+3:]
	}
	return name
}

func commonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	prefix := strs[0]
	for _, s := range strs[1:] {
		for prefix != "" && (len(s) < len(prefix) || s[:len(prefix)] != prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	// Trim trailing spaces, slashes, parens
	for prefix != "" {
		last := prefix[len(prefix)-1]
		if last == ' ' || last == '/' || last == '(' {
			prefix = prefix[:len(prefix)-1]
		} else {
			break
		}
	}
	if len(prefix) < 3 {
		return ""
	}
	return prefix
}
