// Package app contains the application-level orchestration for ci-snitch.
package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/vertti/ci-snitch/internal/analyze"
	"github.com/vertti/ci-snitch/internal/diag"
	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/output"
	"github.com/vertti/ci-snitch/internal/preprocess"
)

// WorkflowFetcher abstracts the GitHub API client.
type WorkflowFetcher interface {
	ListWorkflows(ctx context.Context) ([]model.Workflow, error)
	FetchRuns(ctx context.Context, workflowID int64, since time.Time, branch string) ([]model.WorkflowRun, []diag.Diagnostic, error)
	FetchRunDetails(ctx context.Context, runs []model.WorkflowRun) ([]model.RunDetail, []diag.Diagnostic)
	FetchRunDetailsGraphQL(ctx context.Context, runs []model.WorkflowRun) ([]model.RunDetail, []diag.Diagnostic)
	RateLimit(ctx context.Context) (github.RateLimitStatus, error)
}

// RunStore abstracts the SQLite store.
type RunStore interface {
	RunsSince(workflowID int64, since time.Time) ([]model.WorkflowRun, error)
	IncompleteRunIDs() ([]int64, error)
	LoadRunDetail(runID int64) (*model.RunDetail, error)
	SaveRunDetails(details []model.RunDetail) error
}

// Options configures an analysis run.
type Options struct {
	Repo            string
	Branch          string
	Since           time.Time
	Workflow        string
	IncludeFailures bool
	Verbose         bool
}

// Service orchestrates the fetch → preprocess → analyze pipeline.
type Service struct {
	Client WorkflowFetcher
	Store  RunStore // nil to skip caching
	Prog   *output.Progress
}

// rateLimitSafetyMargin is the fraction of remaining rate limit we refuse to exceed.
const rateLimitSafetyMargin = 0.80

// Run executes the full analysis pipeline and returns the result.
func (s *Service) Run(ctx context.Context, opts *Options) (analyze.AnalysisResult, error) {
	// Fetch workflows
	s.Prog.Status("Discovering workflows...")
	workflows, err := s.Client.ListWorkflows(ctx)
	if err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, fmt.Errorf("list workflows: %w", err)
	}

	targetWorkflows := 0
	for _, wf := range workflows {
		if opts.Workflow == "" || wf.Name == opts.Workflow {
			targetWorkflows++
		}
	}
	if opts.Verbose {
		s.Prog.Log("Found %d workflows (%d targeted)", len(workflows), targetWorkflows)
	}

	// Phase 1: fetch run lists (cheap — paginated listing, no hydration)
	allWfRuns, err := s.fetchRunLists(ctx, workflows, opts)
	if err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, err
	}

	totalRuns := 0
	for i := range allWfRuns {
		totalRuns += len(allWfRuns[i].runs)
	}

	// Estimate API cost and check rate limit budget
	if err := s.checkRateBudget(ctx, totalRuns, opts); err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, err
	}

	// Phase 2: hydrate runs (expensive — 1 API call per uncached run)
	allDetails, err := s.hydrateAll(ctx, allWfRuns, opts)
	if err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, err
	}
	s.Prog.Done()

	if len(allDetails) == 0 {
		return analyze.AnalysisResult{}, fmt.Errorf("no runs found for %s since %s", opts.Repo, opts.Since.Format("2006-01-02"))
	}

	// Compute rerun stats before deduplication (needs to see all attempts)
	rerunStats := preprocess.ComputeRerunStats(allDetails)

	// Deduplicate retried runs for all downstream consumers.
	// This is separate from the dedup inside preprocess.Run — that one only applies
	// to its filtered output, but allDetails is passed directly to the engine.
	allDetails = preprocess.DeduplicateRetries(allDetails)

	// Preprocess: branch filter + failure exclusion
	ppStart := time.Now()
	filtered, ppWarnings := preprocess.Run(allDetails, preprocess.Options{
		Branch:          opts.Branch,
		IncludeFailures: opts.IncludeFailures,
	})
	if opts.Verbose {
		s.Prog.Log("Preprocess: %s", time.Since(ppStart))
	}
	for _, w := range ppWarnings {
		if opts.Verbose {
			s.Prog.Log("Preprocessing: %s", w.Message)
		}
	}

	if len(filtered) == 0 {
		return analyze.AnalysisResult{}, fmt.Errorf("all %d runs were filtered out during preprocessing", len(allDetails))
	}

	s.Prog.Status("Analyzing %d runs...", len(filtered))

	// Run analysis
	analyzeStart := time.Now()
	engine := analyze.NewEngine(analyze.DefaultAnalyzers()...)
	workflowNames := make(map[int64]string, len(workflows))
	for _, wf := range workflows {
		workflowNames[wf.ID] = wf.Name
	}
	result := engine.Run(ctx, filtered, allDetails, rerunStats, workflowNames)
	result.Meta.Repo = opts.Repo
	s.Prog.Done()
	if opts.Verbose {
		s.Prog.Log("Analyze: %s", time.Since(analyzeStart))
	}

	return result, nil
}

type workflowRuns struct {
	wf   model.Workflow
	runs []model.WorkflowRun
}

func (s *Service) fetchRunLists(ctx context.Context, workflows []model.Workflow, opts *Options) ([]workflowRuns, error) {
	var (
		result []workflowRuns
		mu     sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, wf := range workflows {
		if opts.Workflow != "" && wf.Name != opts.Workflow {
			continue
		}
		g.Go(func() error {
			s.Prog.Status("Listing %q...", wf.Name)
			runs, fetchWarnings, err := s.Client.FetchRuns(gctx, wf.ID, opts.Since, opts.Branch)
			if err != nil {
				return fmt.Errorf("fetch runs for %q: %w", wf.Name, err)
			}
			for _, w := range fetchWarnings {
				s.Prog.Log("WARNING: %s", w.Message)
			}
			mu.Lock()
			result = append(result, workflowRuns{wf: wf, runs: runs})
			mu.Unlock()
			return nil
		})
	}
	return result, g.Wait()
}

func (s *Service) hydrateAll(ctx context.Context, allWfRuns []workflowRuns, opts *Options) ([]model.RunDetail, error) {
	var (
		allDetails []model.RunDetail
		mu         sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i := range allWfRuns {
		wr := allWfRuns[i]
		g.Go(func() error {
			details := s.hydrateWorkflow(gctx, wr.wf, wr.runs, opts)
			mu.Lock()
			allDetails = append(allDetails, details...)
			mu.Unlock()
			return nil
		})
	}
	return allDetails, g.Wait()
}

// checkRateBudget estimates API cost and verifies sufficient rate limit remains.
func (s *Service) checkRateBudget(ctx context.Context, totalRuns int, opts *Options) error {
	rl, err := s.Client.RateLimit(ctx)
	if err != nil {
		// Non-fatal: proceed without check if we can't read the rate limit
		if opts.Verbose {
			s.Prog.Log("Could not check rate limit: %v", err)
		}
		return nil
	}

	// Estimate: ~1 API call per run for job hydration + small overhead
	// Cache will reduce this, but we estimate worst-case (no cache hits)
	estimatedCalls := totalRuns + totalRuns/10 // 10% overhead for pagination
	budget := int(float64(rl.Remaining) * rateLimitSafetyMargin)

	if opts.Verbose {
		s.Prog.Log("Rate limit: %d/%d remaining (resets %s), estimated calls: ~%d",
			rl.Remaining, rl.Limit, rl.ResetAt.Format("15:04:05"), estimatedCalls)
	}

	if estimatedCalls > budget {
		return fmt.Errorf(
			"aborting: estimated ~%d API calls for %d runs would exceed rate limit budget "+
				"(%d of %d remaining, resets %s). "+
				"Try a shorter window (--since 7d) or filter to one workflow (--workflow <name>)",
			estimatedCalls, totalRuns, rl.Remaining, rl.Limit,
			time.Until(rl.ResetAt).Round(time.Minute))
	}

	return nil
}

// hydrateWorkflow loads run details from cache or API for a single workflow.
func (s *Service) hydrateWorkflow(ctx context.Context, wf model.Workflow, runs []model.WorkflowRun, opts *Options) []model.RunDetail {
	// Partition runs: serve completed from cache, fetch only new/incomplete from API.
	var details []model.RunDetail
	var needsFetch []model.WorkflowRun

	if s.Store != nil {
		cachedSet := make(map[int64]bool)
		cached, cacheErr := s.Store.RunsSince(wf.ID, opts.Since)
		if cacheErr == nil {
			for i := range cached {
				cachedSet[cached[i].ID] = true
			}
		}

		incompleteSet := make(map[int64]bool)
		incomplete, incErr := s.Store.IncompleteRunIDs()
		if incErr == nil {
			for _, id := range incomplete {
				incompleteSet[id] = true
			}
		}

		for i := range runs {
			if cachedSet[runs[i].ID] && !incompleteSet[runs[i].ID] {
				d, loadErr := s.Store.LoadRunDetail(runs[i].ID)
				if loadErr == nil {
					details = append(details, *d)
					continue
				}
			}
			needsFetch = append(needsFetch, runs[i])
		}

		if opts.Verbose {
			s.Prog.Log("  %q: %d cached, %d to fetch", wf.Name, len(details), len(needsFetch))
		}
	} else {
		needsFetch = runs
	}

	if len(needsFetch) > 0 {
		s.Prog.Status("Fetching %q — hydrating %d runs (%d cached)...", wf.Name, len(needsFetch), len(details))
		hydrateStart := time.Now()
		fetched, warnings := s.Client.FetchRunDetailsGraphQL(ctx, needsFetch)
		if opts.Verbose {
			s.Prog.Log("  %q: hydrated %d runs in %s", wf.Name, len(fetched), time.Since(hydrateStart))
		}
		for _, w := range warnings {
			s.Prog.Log("WARNING: %s", w.Message)
		}

		if s.Store != nil {
			if err := s.Store.SaveRunDetails(fetched); err != nil {
				s.Prog.Log("WARNING: failed to cache %q: %v", wf.Name, err)
			}
		}

		details = append(details, fetched...)
	}

	return details
}
