// Package model defines the core data types for CI workflow analysis.
package model

import "time"

// Workflow represents a GitHub Actions workflow definition.
type Workflow struct {
	ID   int64
	Name string
	Path string
}

// WorkflowRun represents a single execution of a workflow.
type WorkflowRun struct {
	ID           int64
	WorkflowID   int64
	WorkflowName string
	Name         string
	Event        string // trigger type: push, pull_request, schedule, workflow_dispatch, etc.
	Status       string
	Conclusion   string
	HeadBranch   string
	HeadSHA      string
	RunAttempt   int
	CreatedAt    time.Time
	StartedAt    time.Time
	UpdatedAt    time.Time
}

// Duration returns the wall-clock duration of the run.
// Returns zero if timestamps are missing or invalid.
func (r WorkflowRun) Duration() time.Duration {
	if r.StartedAt.IsZero() || r.UpdatedAt.IsZero() {
		return 0
	}
	d := r.UpdatedAt.Sub(r.StartedAt)
	if d < 0 {
		return 0
	}
	return d
}

// IsCompleted reports whether the run has finished.
func (r WorkflowRun) IsCompleted() bool {
	return r.Status == "completed"
}

// Job represents a single job within a workflow run.
type Job struct {
	ID              int64
	RunID           int64
	Name            string
	Status          string
	Conclusion      string
	StartedAt       time.Time
	CompletedAt     time.Time
	RunnerName      string
	RunnerGroupName string
	Labels          []string
	Steps           []Step
}

// Duration returns the wall-clock duration of the job.
func (j Job) Duration() time.Duration {
	if j.StartedAt.IsZero() || j.CompletedAt.IsZero() {
		return 0
	}
	d := j.CompletedAt.Sub(j.StartedAt)
	if d < 0 {
		return 0
	}
	return d
}

// Step represents a single step within a job.
type Step struct {
	Name        string
	Number      int
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

// Duration returns the wall-clock duration of the step.
func (s Step) Duration() time.Duration {
	if s.StartedAt.IsZero() || s.CompletedAt.IsZero() {
		return 0
	}
	d := s.CompletedAt.Sub(s.StartedAt)
	if d < 0 {
		return 0
	}
	return d
}

// RunDetail is a fully hydrated workflow run with its jobs and steps.
type RunDetail struct {
	Run  WorkflowRun
	Jobs []Job
}

// Duration returns the wall-clock duration of the run, preferring job completion
// times over the run's UpdatedAt (which can be bumped by post-completion events).
func (rd RunDetail) Duration() time.Duration {
	var maxCompleted time.Time
	for _, j := range rd.Jobs {
		if !j.CompletedAt.IsZero() && j.CompletedAt.After(maxCompleted) {
			maxCompleted = j.CompletedAt
		}
	}
	if !maxCompleted.IsZero() && !rd.Run.StartedAt.IsZero() {
		d := maxCompleted.Sub(rd.Run.StartedAt)
		if d >= 0 {
			return d
		}
	}
	return rd.Run.Duration()
}
