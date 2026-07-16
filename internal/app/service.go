// Package app contains the application-level orchestration for ci-snitch.
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	GetCommitInfo(ctx context.Context, sha string) (github.CommitInfo, error)
}

// RunStore abstracts the SQLite store.
type RunStore interface {
	RunsSince(workflowID int64, since time.Time) ([]model.WorkflowRun, error)
	IncompleteRunIDs() ([]int64, error)
	LoadRunDetailsByIDs(ids []int64) ([]model.RunDetail, error)
	SaveRunDetails(details []model.RunDetail) error
}

// Options configures an analysis run.
type Options struct {
	Repo            string
	Branch          string
	BranchCategory  string // "pr", "main", or "" (all): selects runs by event
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
	// The client wraps its own errors with context ("list workflows: ...").
	workflows, err := s.Client.ListWorkflows(ctx)
	if err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, err
	}

	targetWorkflows := 0
	for _, wf := range workflows {
		if opts.Workflow == "" || wf.Name == opts.Workflow {
			targetWorkflows++
		}
	}
	if opts.Workflow != "" && targetWorkflows == 0 {
		s.Prog.Done()
		return analyze.AnalysisResult{}, unknownWorkflowError(workflows, opts.Workflow)
	}
	if opts.Verbose {
		s.Prog.Log("Found %d workflows (%d targeted)", len(workflows), targetWorkflows)
	}

	// Diagnostics from fetch/hydrate/preprocess are collected here and attached
	// to the result so every output format (JSON, LLM, ...) sees them — the CLI
	// prints result.Diagnostics to stderr after the run.
	var pipelineDiags []diag.Diagnostic

	// Phase 1: fetch run lists (cheap — paginated listing, no hydration)
	allWfRuns, listDiags, err := s.fetchRunLists(ctx, workflows, opts)
	if err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, err
	}
	pipelineDiags = append(pipelineDiags, listDiags...)

	totalRuns, uncachedRuns := s.countRuns(allWfRuns, opts)

	// Estimate API cost and check rate limit budget
	if err := s.checkRateBudget(ctx, totalRuns, uncachedRuns, opts); err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, err
	}

	// Phase 2: hydrate runs (expensive — 1 API call per uncached run)
	allDetails, hydrateDiags, err := s.hydrateAll(ctx, allWfRuns, opts)
	if err != nil {
		s.Prog.Done()
		return analyze.AnalysisResult{}, err
	}
	pipelineDiags = append(pipelineDiags, hydrateDiags...)
	s.Prog.Done()

	if len(allDetails) == 0 {
		msg := fmt.Sprintf("no runs found for %s since %s", opts.Repo, opts.Since.Format("2006-01-02"))
		if opts.Workflow != "" {
			msg += fmt.Sprintf(" (workflow %q)", opts.Workflow)
		}
		return analyze.AnalysisResult{}, errors.New(msg)
	}

	// Apply run-selection filters (--branch-category, --branch) to the full
	// set so every consumer respects them: failure rates, rerun stats, and
	// cost all read allDetails, not just the duration series.
	allDetails, err = applyRunFilters(allDetails, opts)
	if err != nil {
		return analyze.AnalysisResult{}, err
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
	pipelineDiags = append(pipelineDiags, ppWarnings...)

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
	result.Meta.Branch = opts.Branch
	result.Meta.BranchCategory = opts.BranchCategory
	result.Meta.Workflow = opts.Workflow
	result.Meta.Since = opts.Since
	result.Diagnostics = append(result.Diagnostics, pipelineDiags...)

	// Summarize any jobs missing runner labels (GraphQL doesn't expose them).
	// Emit a single aggregated diagnostic instead of one per workflow/batch.
	if missing := countJobsMissingLabels(allDetails); missing > 0 {
		result.Diagnostics = append(result.Diagnostics, diag.New(
			diag.Info, diag.KindPartialData, "global",
			fmt.Sprintf("runner labels unavailable for %d jobs (cost estimates use default 1x Linux rate)",
				missing),
		))
	}

	s.enrichRegressions(ctx, &result)

	s.Prog.Done()
	if opts.Verbose {
		s.Prog.Log("Analyze: %s", time.Since(analyzeStart))
	}

	return result, nil
}

// maxCommitLookups bounds the per-scan REST spend on change-point context.
const maxCommitLookups = 10

// enrichRegressions annotates confirmed regressions with their commit's
// changed-file stats and a ci-config vs code classification — a workflow-file
// change is the first suspect for a CI regression. Best-effort: lookup
// failures leave the finding unannotated.
func (s *Service) enrichRegressions(ctx context.Context, result *analyze.AnalysisResult) {
	infoBySHA := make(map[string]github.CommitInfo)
	lookups := 0
	for i := range result.Findings {
		if result.Findings[i].Type != analyze.TypeChangepoint {
			continue
		}
		d, ok := result.Findings[i].Detail.(analyze.ChangePointDetail)
		if !ok || d.Category != analyze.CategoryRegression || d.CommitSHA == "" {
			continue
		}
		info, seen := infoBySHA[d.CommitSHA]
		if !seen {
			if lookups >= maxCommitLookups {
				continue
			}
			lookups++
			var err error
			info, err = s.Client.GetCommitInfo(ctx, d.CommitSHA)
			if err != nil {
				continue
			}
			infoBySHA[d.CommitSHA] = info
		}
		d.CommitFilesChanged = info.FilesChanged
		d.CommitAdditions = info.Additions
		d.CommitDeletions = info.Deletions
		d.CommitKind = "code"
		if info.CIConfigChange {
			d.CommitKind = "ci-config"
		}
		result.Findings[i].Detail = d
	}
}

// applyRunFilters narrows allDetails by --branch-category (PR failures are
// expected during development; default-branch failures are incidents) and
// --branch, erroring with the active filter's name when nothing remains.
func applyRunFilters(allDetails []model.RunDetail, opts *Options) ([]model.RunDetail, error) {
	if opts.BranchCategory != "" && opts.BranchCategory != "all" {
		before := len(allDetails)
		allDetails = preprocess.FilterByEventCategory(allDetails, opts.BranchCategory)
		if len(allDetails) == 0 {
			return nil, fmt.Errorf(
				"no runs found for --branch-category %q for %s since %s (%d runs in other categories)",
				opts.BranchCategory, opts.Repo, opts.Since.Format("2006-01-02"), before)
		}
	}
	if opts.Branch != "" {
		before := len(allDetails)
		allDetails = preprocess.FilterByBranch(allDetails, opts.Branch)
		if len(allDetails) == 0 {
			return nil, fmt.Errorf(
				"no runs found for branch %q for %s since %s (%d runs on other branches)",
				opts.Branch, opts.Repo, opts.Since.Format("2006-01-02"), before)
		}
	}
	return allDetails, nil
}

type workflowRuns struct {
	wf   model.Workflow
	runs []model.WorkflowRun
}

// unknownWorkflowError names the typo'd filter and lists what exists —
// silently matching nothing looked like an empty repository.
func unknownWorkflowError(workflows []model.Workflow, filter string) error {
	names := make([]string, 0, len(workflows))
	for _, wf := range workflows {
		names = append(names, wf.Name)
	}
	slices.Sort(names)
	if len(names) > 15 {
		names = append(names[:15], "…")
	}
	return fmt.Errorf("workflow %q not found; available workflows: %s", filter, strings.Join(names, ", "))
}

func countJobsMissingLabels(details []model.RunDetail) int {
	n := 0
	for i := range details {
		for j := range details[i].Jobs {
			if len(details[i].Jobs[j].Labels) == 0 {
				n++
			}
		}
	}
	return n
}

func (s *Service) fetchRunLists(ctx context.Context, workflows []model.Workflow, opts *Options) ([]workflowRuns, []diag.Diagnostic, error) {
	var (
		result []workflowRuns
		diags  []diag.Diagnostic
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
			mu.Lock()
			result = append(result, workflowRuns{wf: wf, runs: runs})
			diags = append(diags, fetchWarnings...)
			mu.Unlock()
			return nil
		})
	}
	return result, diags, g.Wait()
}

func (s *Service) hydrateAll(ctx context.Context, allWfRuns []workflowRuns, opts *Options) ([]model.RunDetail, []diag.Diagnostic, error) {
	var (
		allDetails []model.RunDetail
		diags      []diag.Diagnostic
		mu         sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i := range allWfRuns {
		wr := allWfRuns[i]
		g.Go(func() error {
			details, wfDiags := s.hydrateWorkflow(gctx, wr.wf, wr.runs, opts)
			// A cancelled hydration returns whatever subset it had fetched;
			// analyzing that silently would look like a normal result.
			if err := gctx.Err(); err != nil {
				return err
			}
			mu.Lock()
			allDetails = append(allDetails, details...)
			diags = append(diags, wfDiags...)
			mu.Unlock()
			return nil
		})
	}
	return allDetails, diags, g.Wait()
}

// countRuns returns (totalRuns, uncachedRuns) across all workflows.
// Uncached runs are those that will require an API fetch; cached completed
// runs are served from the local SQLite store.
func (s *Service) countRuns(allWfRuns []workflowRuns, opts *Options) (total, uncached int) {
	// When cache is disabled, everything needs fetching.
	if s.Store == nil {
		for i := range allWfRuns {
			total += len(allWfRuns[i].runs)
		}
		return total, total
	}

	incompleteSet := make(map[int64]bool)
	if incomplete, err := s.Store.IncompleteRunIDs(); err == nil {
		for _, id := range incomplete {
			incompleteSet[id] = true
		}
	}

	for i := range allWfRuns {
		wf := allWfRuns[i].wf
		runs := allWfRuns[i].runs
		total += len(runs)

		cachedAt := s.cachedUpdatedAt(wf.ID, opts.Since)
		for j := range runs {
			if !servableFromCache(cachedAt, incompleteSet, &runs[j]) {
				uncached++
			}
		}
	}
	return total, uncached
}

// cachedUpdatedAt returns run ID → cached UpdatedAt for a workflow's cached
// runs. Read errors degrade to "nothing cached" (a full re-fetch), matching
// the caching-is-best-effort behavior elsewhere.
func (s *Service) cachedUpdatedAt(workflowID int64, since time.Time) map[int64]time.Time {
	cachedAt := make(map[int64]time.Time)
	cached, err := s.Store.RunsSince(workflowID, since)
	if err != nil {
		// Degrades to a full re-fetch — say so instead of silently burning
		// the API budget with no hint why.
		s.Prog.Log("WARNING: cache read failed for workflow %d (%v); re-fetching from API", workflowID, err)
		return cachedAt
	}
	for i := range cached {
		cachedAt[cached[i].ID] = cached[i].UpdatedAt
	}
	return cachedAt
}

// servableFromCache reports whether a listed run can be served from the cache.
// A cached run is stale once the listing shows a newer UpdatedAt — GitHub
// bumps it when a run is re-run, so bare ID membership would serve the old
// attempt forever.
func servableFromCache(cachedAt map[int64]time.Time, incomplete map[int64]bool, run *model.WorkflowRun) bool {
	at, ok := cachedAt[run.ID]
	return ok && !incomplete[run.ID] && !run.UpdatedAt.After(at)
}

// checkRateBudget estimates API cost and verifies sufficient rate limit remains.
// Cost accounts for GraphQL batching: ~1 query per graphqlBatchSize uncached runs.
func (s *Service) checkRateBudget(ctx context.Context, totalRuns, uncachedRuns int, opts *Options) error {
	rl, err := s.Client.RateLimit(ctx)
	if err != nil {
		// Non-fatal: proceed without check if we can't read the rate limit
		if opts.Verbose {
			s.Prog.Log("Could not check rate limit: %v", err)
		}
		return nil
	}

	// GraphQL batches ~20 runs per query. Add 10% overhead for job/step pagination.
	estimatedCalls := uncachedRuns / github.GraphQLBatchSize
	if uncachedRuns%github.GraphQLBatchSize != 0 {
		estimatedCalls++
	}
	estimatedCalls += estimatedCalls / 10

	// Hydration runs on the GraphQL pool, which GitHub meters separately from
	// core REST (already spent on listing by the time this check runs). Fall
	// back to the core pool only when the API exposes no GraphQL pool.
	pool, poolName := rl.GraphQL, "GraphQL"
	if pool.Limit == 0 {
		pool, poolName = rl.Core, "core"
	}
	budget := int(float64(pool.Remaining) * rateLimitSafetyMargin)

	if opts.Verbose {
		s.Prog.Log("Rate limit (%s pool): %d/%d remaining (resets %s). %d runs (%d cached, %d to fetch), estimated calls: ~%d",
			poolName, pool.Remaining, pool.Limit, pool.ResetAt.Format("15:04:05"),
			totalRuns, totalRuns-uncachedRuns, uncachedRuns, estimatedCalls)
	}

	if estimatedCalls > budget {
		return fmt.Errorf(
			"aborting: estimated ~%d GraphQL calls for %d uncached runs (of %d total) would exceed the %s rate limit budget "+
				"(%d of %d remaining, resets %s). "+
				"Try a shorter window (--since 7d) or filter to one workflow (--workflow <name>)",
			estimatedCalls, uncachedRuns, totalRuns, poolName, pool.Remaining, pool.Limit,
			time.Until(pool.ResetAt).Round(time.Minute))
	}

	return nil
}

// partitionCached splits runs into cache-served details and runs needing a
// fetch. Servable runs hydrate in one batch call (3 queries) instead of
// 1 + jobs queries per run; anything the batch cannot produce is re-fetched
// (caching is best-effort).
func (s *Service) partitionCached(wf model.Workflow, runs []model.WorkflowRun, opts *Options) (details []model.RunDetail, needsFetch []model.WorkflowRun) {
	cachedAt := s.cachedUpdatedAt(wf.ID, opts.Since)

	incompleteSet := make(map[int64]bool)
	if incomplete, err := s.Store.IncompleteRunIDs(); err == nil {
		for _, id := range incomplete {
			incompleteSet[id] = true
		}
	}

	var servable []model.WorkflowRun
	var servableIDs []int64
	for i := range runs {
		if servableFromCache(cachedAt, incompleteSet, &runs[i]) {
			servable = append(servable, runs[i])
			servableIDs = append(servableIDs, runs[i].ID)
		} else {
			needsFetch = append(needsFetch, runs[i])
		}
	}
	if len(servable) == 0 {
		return details, needsFetch
	}

	cached, err := s.Store.LoadRunDetailsByIDs(servableIDs)
	if err != nil {
		return details, append(needsFetch, servable...)
	}
	loadedSet := make(map[int64]bool, len(cached))
	for i := range cached {
		loadedSet[cached[i].Run.ID] = true
	}
	details = append(details, cached...)
	for i := range servable {
		if !loadedSet[servable[i].ID] {
			needsFetch = append(needsFetch, servable[i])
		}
	}
	return details, needsFetch
}

// hydrateWorkflow loads run details from cache or API for a single workflow.
func (s *Service) hydrateWorkflow(ctx context.Context, wf model.Workflow, runs []model.WorkflowRun, opts *Options) ([]model.RunDetail, []diag.Diagnostic) {
	// Partition runs: serve completed from cache, fetch only new/incomplete from API.
	var details []model.RunDetail
	var diags []diag.Diagnostic
	var needsFetch []model.WorkflowRun

	if s.Store != nil {
		details, needsFetch = s.partitionCached(wf, runs, opts)

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
		diags = append(diags, warnings...)

		if s.Store != nil {
			// Truncated details (jobs/steps beyond the GraphQL per-query
			// limit) are analyzed but never cached: a cached row would serve
			// the incomplete data forever.
			cacheable := make([]model.RunDetail, 0, len(fetched))
			for i := range fetched {
				if !fetched[i].Truncated {
					cacheable = append(cacheable, fetched[i])
				}
			}
			if err := s.Store.SaveRunDetails(cacheable); err != nil {
				diags = append(diags, diag.Errorf(diag.KindCache, wf.Name, err,
					"failed to cache %d runs for %q: %v (they will be re-fetched next run)",
					len(cacheable), wf.Name, err))
			}
		}

		details = append(details, fetched...)
	}

	return details, diags
}
