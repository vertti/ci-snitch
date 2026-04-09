package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWorkflowRun_Duration(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		run     WorkflowRun
		wantDur time.Duration
	}{
		{
			name: "normal duration",
			run: WorkflowRun{
				StartedAt: base,
				UpdatedAt: base.Add(5 * time.Minute),
			},
			wantDur: 5 * time.Minute,
		},
		{
			name:    "zero start",
			run:     WorkflowRun{UpdatedAt: base},
			wantDur: 0,
		},
		{
			name:    "zero end",
			run:     WorkflowRun{StartedAt: base},
			wantDur: 0,
		},
		{
			name:    "both zero",
			run:     WorkflowRun{},
			wantDur: 0,
		},
		{
			name: "negative duration returns zero",
			run: WorkflowRun{
				StartedAt: base.Add(5 * time.Minute),
				UpdatedAt: base,
			},
			wantDur: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantDur, tt.run.Duration())
		})
	}
}

func TestJob_Duration(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		job     Job
		wantDur time.Duration
	}{
		{
			name: "normal duration",
			job: Job{
				StartedAt:   base,
				CompletedAt: base.Add(3 * time.Minute),
			},
			wantDur: 3 * time.Minute,
		},
		{
			name:    "missing start",
			job:     Job{CompletedAt: base},
			wantDur: 0,
		},
		{
			name:    "missing end",
			job:     Job{StartedAt: base},
			wantDur: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantDur, tt.job.Duration())
		})
	}
}

func TestStep_Duration(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		step    Step
		wantDur time.Duration
	}{
		{
			name: "normal duration",
			step: Step{
				StartedAt:   base,
				CompletedAt: base.Add(10 * time.Second),
			},
			wantDur: 10 * time.Second,
		},
		{
			name: "zero duration step",
			step: Step{
				StartedAt:   base,
				CompletedAt: base,
			},
			wantDur: 0,
		},
		{
			name:    "missing timestamps",
			step:    Step{},
			wantDur: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantDur, tt.step.Duration())
		})
	}
}

func TestWorkflowRun_IsCompleted(t *testing.T) {
	assert.True(t, WorkflowRun{Status: "completed"}.IsCompleted())
	assert.False(t, WorkflowRun{Status: "in_progress"}.IsCompleted())
	assert.False(t, WorkflowRun{Status: "queued"}.IsCompleted())
	assert.False(t, WorkflowRun{Status: ""}.IsCompleted())
}
