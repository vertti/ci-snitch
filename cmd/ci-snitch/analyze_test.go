package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/app"
	"github.com/vertti/ci-snitch/internal/diag"
	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/output"
)

func TestParseSinceFrom(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		input   string
		want    time.Time
		wantErr string
	}{
		{name: "absolute date", input: "2026-01-01", want: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "days", input: "60d", want: now.AddDate(0, 0, -60)},
		{name: "weeks", input: "2w", want: now.AddDate(0, 0, -14)},
		{name: "months", input: "3mo", want: now.AddDate(0, -3, 0)},
		{name: "single day", input: "1d", want: now.AddDate(0, 0, -1)},
		{name: "single month", input: "1mo", want: now.AddDate(0, -1, 0)},
		{name: "too short", input: "x", wantErr: "unrecognized format"},
		{name: "empty", input: "", wantErr: "unrecognized format"},
		{name: "bad suffix", input: "5y", wantErr: "unrecognized format"},
		{name: "bad number days", input: "abcd", wantErr: "unrecognized format"},
		{name: "bad number months", input: "abmo", wantErr: "unrecognized format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSinceFrom(tt.input, now)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGitHubRemoteRe(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
		ok   bool
	}{
		{name: "ssh", url: "git@github.com:vertti/ci-snitch.git", want: "vertti/ci-snitch", ok: true},
		{name: "https", url: "https://github.com/vertti/ci-snitch.git", want: "vertti/ci-snitch", ok: true},
		{name: "https no .git", url: "https://github.com/vertti/ci-snitch", want: "vertti/ci-snitch", ok: true},
		{name: "ssh no .git", url: "git@github.com:org/repo", want: "org/repo", ok: true},
		{name: "not github", url: "git@gitlab.com:org/repo.git", ok: false},
		{name: "bare path", url: "/tmp/some-repo", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := gitHubRemoteRe.FindStringSubmatch(tt.url)
			if !tt.ok {
				assert.Nil(t, m)
				return
			}
			require.NotNil(t, m)
			assert.Equal(t, tt.want, m[1])
		})
	}
}

// stubFetcher implements app.WorkflowFetcher for testing.
type stubFetcher struct {
	workflows []model.Workflow
	runs      []model.WorkflowRun
	details   []model.RunDetail
}

func (f *stubFetcher) ListWorkflows(_ context.Context) ([]model.Workflow, error) {
	return f.workflows, nil
}

func (f *stubFetcher) FetchRuns(_ context.Context, _ int64, _ time.Time, _ string) ([]model.WorkflowRun, []diag.Diagnostic, error) {
	return f.runs, nil, nil
}

func (f *stubFetcher) FetchRunDetails(_ context.Context, _ []model.WorkflowRun) ([]model.RunDetail, []diag.Diagnostic) {
	return f.details, nil
}

func (f *stubFetcher) RateLimit(_ context.Context) (github.RateLimitStatus, error) {
	return github.RateLimitStatus{Remaining: 5000, Limit: 5000, ResetAt: time.Now().Add(time.Hour)}, nil
}

func TestServiceRun_BasicPipeline(t *testing.T) {
	now := time.Now()
	wf := model.Workflow{ID: 1, Name: "CI"}
	run := model.WorkflowRun{
		ID: 100, WorkflowID: 1, WorkflowName: "CI",
		Status: "completed", Conclusion: "success",
		HeadBranch: "main", HeadSHA: "abc123",
		RunAttempt: 1,
		CreatedAt:  now.Add(-1 * time.Hour),
		StartedAt:  now.Add(-1 * time.Hour),
		UpdatedAt:  now.Add(-30 * time.Minute),
	}
	detail := model.RunDetail{
		Run: run,
		Jobs: []model.Job{{
			ID: 200, RunID: 100, Name: "build",
			Status: "completed", Conclusion: "success",
			StartedAt:   now.Add(-1 * time.Hour),
			CompletedAt: now.Add(-30 * time.Minute),
			Labels:      []string{"ubuntu-latest"},
		}},
	}

	svc := &app.Service{
		Client: &stubFetcher{
			workflows: []model.Workflow{wf},
			runs:      []model.WorkflowRun{run},
			details:   []model.RunDetail{detail},
		},
		Prog: output.NewProgress(),
	}

	result, err := svc.Run(context.Background(), &app.Options{
		Repo:  "test/repo",
		Since: now.Add(-7 * 24 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Meta.TotalRuns)
	assert.Equal(t, "test/repo", result.Meta.Repo)
	assert.NotEmpty(t, result.Findings)
}

func TestServiceRun_NoRunsError(t *testing.T) {
	svc := &app.Service{
		Client: &stubFetcher{
			workflows: []model.Workflow{{ID: 1, Name: "CI"}},
		},
		Prog: output.NewProgress(),
	}

	_, err := svc.Run(context.Background(), &app.Options{
		Repo:  "test/repo",
		Since: time.Now().Add(-7 * 24 * time.Hour),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no runs found")
}

func TestServiceRun_WorkflowFilter(t *testing.T) {
	now := time.Now()
	run := model.WorkflowRun{
		ID: 100, WorkflowID: 2, WorkflowName: "Deploy",
		Status: "completed", Conclusion: "success",
		HeadBranch: "main", HeadSHA: "abc123", RunAttempt: 1,
		CreatedAt: now.Add(-1 * time.Hour),
		StartedAt: now.Add(-1 * time.Hour),
		UpdatedAt: now.Add(-30 * time.Minute),
	}
	detail := model.RunDetail{
		Run: run,
		Jobs: []model.Job{{
			ID: 200, RunID: 100, Name: "deploy",
			Status: "completed", Conclusion: "success",
			StartedAt: now.Add(-1 * time.Hour), CompletedAt: now.Add(-30 * time.Minute),
			Labels: []string{"ubuntu-latest"},
		}},
	}

	svc := &app.Service{
		Client: &stubFetcher{
			workflows: []model.Workflow{
				{ID: 1, Name: "CI"},
				{ID: 2, Name: "Deploy"},
			},
			runs:    []model.WorkflowRun{run},
			details: []model.RunDetail{detail},
		},
		Prog: output.NewProgress(),
	}

	result, err := svc.Run(context.Background(), &app.Options{
		Repo:     "test/repo",
		Workflow: "Deploy",
		Since:    now.Add(-7 * 24 * time.Hour),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Meta.TotalRuns)
}
