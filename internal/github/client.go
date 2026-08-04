package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v90/github"
	"github.com/vertti/ci-snitch/internal/diag"
	"github.com/vertti/ci-snitch/internal/model"
)

const defaultMaxConcurrentJobs = 20

// Client wraps the GitHub API for fetching Actions workflow data.
type Client struct {
	gh         *gh.Client
	owner      string
	repo       string
	jobSem     chan struct{}
	logger     *slog.Logger
	graphqlURL string
}

// ClientOption configures optional Client behaviour.
type ClientOption func(*Client)

// WithLogger sets a structured logger for the client.
func WithLogger(l *slog.Logger) ClientOption {
	return func(c *Client) {
		c.logger = l
	}
}

// NewClient creates a Client for the given owner/repo.
func NewClient(token, ownerRepo string, opts ...ClientOption) (*Client, error) {
	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid repo format %q, expected owner/repo", ownerRepo)
	}

	ghClient, err := gh.NewClient(gh.WithAuthToken(token))
	if err != nil {
		return nil, fmt.Errorf("new github client: %w", err)
	}
	c := &Client{
		gh:         ghClient,
		owner:      parts[0],
		repo:       parts[1],
		jobSem:     make(chan struct{}, defaultMaxConcurrentJobs),
		graphqlURL: graphqlEndpoint,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func (c *Client) log(msg string, args ...any) {
	if c.logger != nil {
		c.logger.Info(msg, args...)
	}
}

// classifyAPIError turns raw go-github status errors into guidance the user
// can act on ("GET https://…: 404 Not Found []" says nothing about tokens).
func (c *Client) classifyAPIError(err error) error {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return err
	}
	switch ghErr.Response.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("repository %s/%s not found — check the spelling; if it is private, your token needs access to it (%w)", c.owner, c.repo, err)
	case http.StatusUnauthorized:
		return fmt.Errorf("bad credentials — the token is invalid or expired; run `gh auth login` or refresh GITHUB_TOKEN (%w)", err)
	case http.StatusForbidden:
		return fmt.Errorf("access denied — the token may lack the repo scope or need SAML SSO authorization for this organization (%w)", err)
	default:
		return err
	}
}

// RatePool describes one rate-limit pool (core REST or GraphQL — GitHub
// meters them separately).
type RatePool struct {
	Remaining int
	Limit     int
	ResetAt   time.Time
}

// RateLimitStatus carries the current state of both rate-limit pools.
type RateLimitStatus struct {
	Core    RatePool
	GraphQL RatePool
}

// RateLimit returns the current GitHub API rate limit status for both the
// core REST pool and the GraphQL pool.
func (c *Client) RateLimit(ctx context.Context) (RateLimitStatus, error) {
	limits, _, err := c.gh.RateLimit.Get(ctx)
	if err != nil {
		return RateLimitStatus{}, fmt.Errorf("get rate limit: %w", err)
	}
	status := RateLimitStatus{}
	if core := limits.Core; core != nil {
		status.Core = RatePool{Remaining: core.Remaining, Limit: core.Limit, ResetAt: core.Reset.Time}
	}
	if gql := limits.GraphQL; gql != nil {
		status.GraphQL = RatePool{Remaining: gql.Remaining, Limit: gql.Limit, ResetAt: gql.Reset.Time}
	}
	return status, nil
}

// ListWorkflows returns all workflows in the repository.
func (c *Client) ListWorkflows(ctx context.Context) ([]model.Workflow, error) {
	var all []model.Workflow
	opts := &gh.ListOptions{PerPage: 100}

	for {
		result, resp, err := c.gh.Actions.ListWorkflows(ctx, c.owner, c.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list workflows: %w", c.classifyAPIError(err))
		}

		for _, w := range result.Workflows {
			all = append(all, model.Workflow{
				ID:   w.GetID(),
				Name: w.GetName(),
				Path: w.GetPath(),
			})
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return all, nil
}

// CommitInfo summarizes a commit for change-point attribution.
type CommitInfo struct {
	FilesChanged   int
	Additions      int
	Deletions      int
	CIConfigChange bool // any file under .github/workflows/
}

// GetCommitInfo fetches a commit's changed files and classifies whether it
// touched CI configuration. Used to annotate change points (one bounded call
// per detected regression).
func (c *Client) GetCommitInfo(ctx context.Context, sha string) (CommitInfo, error) {
	commit, _, err := c.gh.Repositories.GetCommit(ctx, c.owner, c.repo, sha, &gh.ListOptions{PerPage: 100})
	if err != nil {
		// A 404 here is not the repo-not-found case classifyAPIError
		// describes: the SHA itself is unreachable (force-push, GC).
		var ghErr *gh.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return CommitInfo{}, fmt.Errorf("commit %s not found — the ref may have been rewritten or garbage-collected (%w)", sha, err)
		}
		return CommitInfo{}, fmt.Errorf("get commit %s: %w", sha, c.classifyAPIError(err))
	}
	info := CommitInfo{FilesChanged: len(commit.Files)}
	if commit.Stats != nil {
		info.Additions = commit.Stats.GetAdditions()
		info.Deletions = commit.Stats.GetDeletions()
	}
	for _, f := range commit.Files {
		if strings.HasPrefix(f.GetFilename(), ".github/workflows/") {
			info.CIConfigChange = true
			break
		}
	}
	return info, nil
}

// dateWindowSize is the number of days per sliding window when fetching runs.
// Kept small to avoid the GitHub API 1,000-result cap on filtered queries.
const dateWindowSize = 7

// FetchRuns fetches completed workflow runs for a specific workflow since the given time.
// Uses sliding date windows to avoid the GitHub API 1,000-result cap.
// If branch is empty, runs from all branches are returned.
func (c *Client) FetchRuns(ctx context.Context, workflowID int64, since time.Time, branch string) ([]model.WorkflowRun, []diag.Diagnostic, error) {
	var all []model.WorkflowRun
	var warnings []diag.Diagnostic
	now := time.Now().UTC()
	windowStart := since

	for windowStart.Before(now) {
		windowEnd := windowStart.AddDate(0, 0, dateWindowSize)
		if windowEnd.After(now) {
			windowEnd = now
		}

		runs, windowWarnings, err := c.fetchRunsWindow(ctx, workflowID, windowStart, windowEnd, branch)
		if err != nil {
			return all, warnings, fmt.Errorf("fetch runs for window %s..%s: %w",
				windowStart.Format("2006-01-02"), windowEnd.Format("2006-01-02"), err)
		}
		all = append(all, runs...)
		warnings = append(warnings, windowWarnings...)

		// The created filter is date-only and inclusive on both ends: the
		// next window must start the day AFTER this one ends, or the seam
		// day is listed (and hydrated, budgeted, saved) twice.
		windowStart = windowEnd.AddDate(0, 0, 1)
	}

	return all, warnings, nil
}

func (c *Client) fetchRunsWindow(ctx context.Context, workflowID int64, start, end time.Time, branch string) ([]model.WorkflowRun, []diag.Diagnostic, error) {
	var all []model.WorkflowRun
	var warnings []diag.Diagnostic
	created := fmt.Sprintf("%s..%s", start.Format("2006-01-02"), end.Format("2006-01-02"))

	opts := &gh.ListWorkflowRunsOptions{
		Status:  "completed",
		Created: created,
		ListOptions: gh.ListOptions{
			PerPage: 100,
		},
	}
	if branch != "" {
		opts.Branch = branch
	}

	for {
		result, resp, err := c.gh.Actions.ListWorkflowRunsByID(ctx, c.owner, c.repo, workflowID, opts)
		if err != nil {
			return nil, nil, c.classifyAPIError(err)
		}

		// Warn once per window (opts.Page is 0 only on the first page).
		if opts.Page == 0 && result.GetTotalCount() > 1000 {
			warnings = append(warnings, diag.New(
				diag.Warn, diag.KindPartialData, fmt.Sprintf("workflow-%d", workflowID),
				fmt.Sprintf("has %d runs in window %s, results may be truncated (GitHub API cap is 1000)",
					result.GetTotalCount(), created),
			))
		}

		for _, r := range result.WorkflowRuns {
			all = append(all, convertRun(r))
		}

		if resp.NextPage == 0 {
			break
		}

		if err := c.waitForRateReset(ctx, resp); err != nil {
			return all, warnings, err
		}

		opts.Page = resp.NextPage
	}

	return all, warnings, nil
}

// restRateFloor is the remaining-call low-water mark below which paginated
// REST loops sleep until the pool resets, leaving headroom for other
// consumers of the same token.
const restRateFloor = 100

// waitForRateReset sleeps until the REST rate limit resets when the pool is
// nearly exhausted. Returns early with ctx.Err() on cancellation.
func (c *Client) waitForRateReset(ctx context.Context, resp *gh.Response) error {
	remaining := resp.Rate.Remaining
	if remaining >= restRateFloor {
		return nil
	}
	wait := time.Until(resp.Rate.Reset.Time)
	if wait <= 0 {
		return nil
	}
	c.log("Rate limit low, sleeping until reset",
		"remaining", remaining, "wait", wait.Round(time.Second))
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// defaultWorkers is the number of goroutines dispatching job-fetch work.
// Effective concurrency is bounded by the Client's jobSem semaphore.
const defaultWorkers = 20

// FetchJobs fetches jobs and steps for a single workflow run.
func (c *Client) FetchJobs(ctx context.Context, runID int64) ([]model.Job, error) {
	// Acquire semaphore slot to bound total concurrent API calls.
	select {
	case c.jobSem <- struct{}{}:
		defer func() { <-c.jobSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var all []model.Job
	opts := &gh.ListOptions{PerPage: 100}

	for {
		result, resp, err := c.gh.Actions.ListWorkflowJobs(ctx, c.owner, c.repo, runID, &gh.ListWorkflowJobsOptions{
			Filter:      "latest",
			ListOptions: *opts,
		})
		if err != nil {
			return nil, fmt.Errorf("list jobs for run %d: %w", runID, c.classifyAPIError(err))
		}

		for _, j := range result.Jobs {
			all = append(all, convertJob(j))
		}

		if resp.NextPage == 0 {
			break
		}

		if err := c.waitForRateReset(ctx, resp); err != nil {
			return all, err
		}

		opts.Page = resp.NextPage
	}

	return all, nil
}

// FetchRunDetails hydrates a slice of workflow runs with their jobs and steps.
// Uses a worker pool for bounded concurrency. Returns partial results and
// warnings for runs that failed to fetch.
func (c *Client) FetchRunDetails(ctx context.Context, runs []model.WorkflowRun) (details []model.RunDetail, warnings []diag.Diagnostic) {
	type result struct {
		detail model.RunDetail
		warn   *diag.Diagnostic
	}

	work := make(chan model.WorkflowRun, len(runs))
	results := make(chan result, len(runs))

	workers := min(defaultWorkers, len(runs))

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for run := range work {
				jobs, err := c.FetchJobs(ctx, run.ID)
				if err != nil {
					// Cancellation is not a per-run fetch failure; the caller
					// observes ctx.Err() and aborts.
					if ctx.Err() != nil {
						continue
					}
					results <- result{
						warn: &diag.Diagnostic{
							Severity: diag.Warn,
							Kind:     diag.KindNetwork,
							Scope:    fmt.Sprintf("run-%d", run.ID),
							Message:  fmt.Sprintf("failed to fetch jobs for run %d", run.ID),
							Err:      err,
						},
					}
					continue
				}
				results <- result{
					detail: model.RunDetail{Run: run, Jobs: jobs},
				}
			}
		})
	}

	for i := range runs {
		work <- runs[i]
	}
	close(work)

	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		if r.warn != nil {
			warnings = append(warnings, *r.warn)
		} else {
			details = append(details, r.detail)
		}
	}

	return details, warnings
}

func convertJob(j *gh.WorkflowJob) model.Job {
	job := model.Job{
		ID:              j.GetID(),
		RunID:           j.GetRunID(),
		Name:            j.GetName(),
		Status:          j.GetStatus(),
		Conclusion:      j.GetConclusion(),
		StartedAt:       j.GetStartedAt().Time,
		RunnerName:      j.GetRunnerName(),
		RunnerGroupName: j.GetRunnerGroupName(),
		Labels:          j.Labels,
	}
	if j.CompletedAt != nil {
		job.CompletedAt = j.CompletedAt.Time
	}

	for _, s := range j.Steps {
		step := model.Step{
			Name:       s.GetName(),
			Number:     int(s.GetNumber()),
			Status:     s.GetStatus(),
			Conclusion: s.GetConclusion(),
		}
		if s.StartedAt != nil {
			step.StartedAt = s.StartedAt.Time
		}
		if s.CompletedAt != nil {
			step.CompletedAt = s.CompletedAt.Time
		}
		job.Steps = append(job.Steps, step)
	}

	return job
}

func convertRun(r *gh.WorkflowRun) model.WorkflowRun {
	return model.WorkflowRun{
		ID:           r.GetID(),
		NodeID:       r.GetNodeID(),
		WorkflowID:   r.GetWorkflowID(),
		WorkflowName: r.GetName(),
		Name:         r.GetDisplayTitle(),
		Event:        r.GetEvent(),
		Status:       r.GetStatus(),
		Conclusion:   r.GetConclusion(),
		HeadBranch:   r.GetHeadBranch(),
		HeadSHA:      r.GetHeadSHA(),
		RunAttempt:   r.GetRunAttempt(),
		CreatedAt:    r.GetCreatedAt().Time,
		StartedAt:    r.GetRunStartedAt().Time,
		UpdatedAt:    r.GetUpdatedAt().Time,
	}
}
