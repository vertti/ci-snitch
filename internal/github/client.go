package github

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/vertti/ci-snitch/internal/model"
)

// Client wraps the GitHub API for fetching Actions workflow data.
type Client struct {
	gh    *gh.Client
	owner string
	repo  string
}

// NewClient creates a Client for the given owner/repo.
func NewClient(token, ownerRepo string) (*Client, error) {
	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid repo format %q, expected owner/repo", ownerRepo)
	}

	return &Client{
		gh:    gh.NewClient(nil).WithAuthToken(token),
		owner: parts[0],
		repo:  parts[1],
	}, nil
}

// ListWorkflows returns all workflows in the repository.
func (c *Client) ListWorkflows(ctx context.Context) ([]model.Workflow, error) {
	var all []model.Workflow
	opts := &gh.ListOptions{PerPage: 100}

	for {
		result, resp, err := c.gh.Actions.ListWorkflows(ctx, c.owner, c.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list workflows: %w", err)
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

// dateWindowSize is the number of days per sliding window when fetching runs.
// Kept small to avoid the GitHub API 1,000-result cap on filtered queries.
const dateWindowSize = 7

// FetchRuns fetches completed workflow runs for a specific workflow since the given time.
// Uses sliding date windows to avoid the GitHub API 1,000-result cap.
// If branch is empty, runs from all branches are returned.
func (c *Client) FetchRuns(ctx context.Context, workflowID int64, since time.Time, branch string) ([]model.WorkflowRun, error) {
	var all []model.WorkflowRun
	now := time.Now().UTC()
	windowStart := since

	for windowStart.Before(now) {
		windowEnd := windowStart.AddDate(0, 0, dateWindowSize)
		if windowEnd.After(now) {
			windowEnd = now
		}

		runs, err := c.fetchRunsWindow(ctx, workflowID, windowStart, windowEnd, branch)
		if err != nil {
			return all, fmt.Errorf("fetch runs for window %s..%s: %w",
				windowStart.Format("2006-01-02"), windowEnd.Format("2006-01-02"), err)
		}
		all = append(all, runs...)

		windowStart = windowEnd
	}

	return all, nil
}

func (c *Client) fetchRunsWindow(ctx context.Context, workflowID int64, start, end time.Time, branch string) ([]model.WorkflowRun, error) {
	var all []model.WorkflowRun
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
			return nil, err
		}

		if result.GetTotalCount() > 1000 {
			log.Printf("WARNING: workflow %d has %d runs in window %s, results may be truncated (GitHub API cap is 1000)",
				workflowID, result.GetTotalCount(), created)
		}

		for _, r := range result.WorkflowRuns {
			all = append(all, convertRun(r))
		}

		if resp.NextPage == 0 {
			break
		}

		remaining := resp.Rate.Remaining
		if remaining < 100 {
			sleepUntil := resp.Rate.Reset.Time
			wait := time.Until(sleepUntil)
			if wait > 0 {
				log.Printf("Rate limit low (%d remaining), sleeping %s until reset", remaining, wait.Round(time.Second))
				select {
				case <-ctx.Done():
					return all, ctx.Err()
				case <-time.After(wait):
				}
			}
		}

		opts.Page = resp.NextPage
	}

	return all, nil
}

func convertRun(r *gh.WorkflowRun) model.WorkflowRun {
	return model.WorkflowRun{
		ID:           r.GetID(),
		WorkflowID:   r.GetWorkflowID(),
		WorkflowName: r.GetName(),
		Name:         r.GetDisplayTitle(),
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
