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
	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/output"
	"github.com/vertti/ci-snitch/internal/preprocess"
)

// WorkflowFetcher abstracts the GitHub API client.
type WorkflowFetcher interface {
	ListWorkflows(ctx context.Context) ([]model.Workflow, error)
	FetchRuns(ctx context.Context, workflowID int64, since time.Time, branch string) ([]model.WorkflowRun, []diag.Diagnostic, error)
	FetchRunDetails(ctx context.Context, runs []model.WorkflowRun) ([]model.RunDetail, []diag.Diagnostic)
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

// Run executes the full analysis pipeline and returns the result.
func (s *Service) Run(ctx context.Context, opts Options) (analyze.AnalysisResult, error) {
	// Fetch workflows
	s.Prog.Status("Discovering workflows...")
	workflows, err := s.Client.ListWorkflows(ctx)
	if err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, fmt.Errorf("list workflows: %w", err)
	}
	if opts.Verbose {
		s.Prog.Log("Found %d workflows", len(workflows))
	}

	// Collect all run details (parallel across workflows)
	var (
		allDetails []model.RunDetail
		mu         sync.Mutex
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, wf := range workflows {
		if opts.Workflow != "" && wf.Name != opts.Workflow {
			continue
		}

		g.Go(func() error {
			details, err := s.fetchWorkflow(gctx, wf, opts)
			if err != nil {
				return err
			}
			mu.Lock()
			allDetails = append(allDetails, details...)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
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

// fetchWorkflow fetches and hydrates runs for a single workflow, using the cache when available.
func (s *Service) fetchWorkflow(ctx context.Context, wf model.Workflow, opts Options) ([]model.RunDetail, error) {
	s.Prog.Status("Fetching %q...", wf.Name)
	fetchStart := time.Now()
	runs, fetchWarnings, err := s.Client.FetchRuns(ctx, wf.ID, opts.Since, opts.Branch)
	if err != nil {
		return nil, fmt.Errorf("fetch runs for %q: %w", wf.Name, err)
	}
	for _, w := range fetchWarnings {
		s.Prog.Log("WARNING: %s", w.Message)
	}
	if opts.Verbose {
		s.Prog.Log("  %q: fetched %d runs in %s", wf.Name, len(runs), time.Since(fetchStart))
	}

	// Partition runs: serve completed from cache, fetch only new/incomplete from API.
	var details []model.RunDetail
	var needsFetch []model.WorkflowRun

	if s.Store != nil {
		cachedSet := make(map[int64]bool)
		cached, cacheErr := s.Store.RunsSince(wf.ID, opts.Since)
		if cacheErr == nil {
			for _, r := range cached {
				cachedSet[r.ID] = true
			}
		}

		incompleteSet := make(map[int64]bool)
		incomplete, incErr := s.Store.IncompleteRunIDs()
		if incErr == nil {
			for _, id := range incomplete {
				incompleteSet[id] = true
			}
		}

		for _, r := range runs {
			if cachedSet[r.ID] && !incompleteSet[r.ID] {
				d, loadErr := s.Store.LoadRunDetail(r.ID)
				if loadErr == nil {
					details = append(details, *d)
					continue
				}
			}
			needsFetch = append(needsFetch, r)
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
		fetched, warnings := s.Client.FetchRunDetails(ctx, needsFetch)
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

	return details, nil
}
