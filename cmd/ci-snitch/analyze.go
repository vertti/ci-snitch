package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/vertti/ci-snitch/internal/analyze"
	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/output"
	"github.com/vertti/ci-snitch/internal/preprocess"
	"github.com/vertti/ci-snitch/internal/store"
	"github.com/vertti/ci-snitch/internal/system"
)

// workflowFetcher abstracts the GitHub API client for testability.
type workflowFetcher interface {
	ListWorkflows(ctx context.Context) ([]model.Workflow, error)
	FetchRuns(ctx context.Context, workflowID int64, since time.Time, branch string) ([]model.WorkflowRun, []github.Warning, error)
	FetchRunDetails(ctx context.Context, runs []model.WorkflowRun) ([]model.RunDetail, []github.Warning)
}

// runStore abstracts the SQLite store for testability.
type runStore interface {
	RunsSince(workflowID int64, since time.Time) ([]model.WorkflowRun, error)
	IncompleteRunIDs() ([]int64, error)
	LoadRunDetail(runID int64) (*model.RunDetail, error)
	SaveRunDetails(details []model.RunDetail) error
	Close() error
}

func newAnalyzeCmd() *cobra.Command {
	var (
		branch          string
		since           string
		workflow        string
		format          string
		rawOutput       string
		noCache         bool
		includeFailures bool
		verbose         bool
	)

	cmd := &cobra.Command{
		Use:   "analyze [owner/repo]",
		Short: "Analyze CI workflow performance",
		Long: `Fetch workflow run data and compute performance statistics, outliers, and trends.

If no repository is specified, detects the GitHub remote from the current directory.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var repo string
			if len(args) > 0 {
				repo = args[0]
			} else {
				detected, err := detectGitHubRepo()
				if err != nil {
					cmd.SilenceUsage = true
					return errors.New("provide a repository: ci-snitch analyze <owner/repo>\nor run from inside a GitHub repo directory")
				}
				repo = detected
			}
			return runAnalyze(cmd, analyzeOpts{
				repo:            repo,
				branch:          branch,
				since:           since,
				workflow:        workflow,
				format:          format,
				rawOutput:       rawOutput,
				noCache:         noCache,
				includeFailures: includeFailures,
				verbose:         verbose,
			})
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "filter to this branch (default: all branches)")
	cmd.Flags().StringVar(&since, "since", "60d", "how far back to analyze (e.g. 60d, 2026-01-01)")
	cmd.Flags().StringVar(&workflow, "workflow", "", "filter to this workflow name")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json, markdown, llm")
	cmd.Flags().StringVar(&rawOutput, "raw-output", "", "write full JSON to file (useful with --format llm to keep report compact)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "bypass local cache, fetch fresh data")
	cmd.Flags().BoolVar(&includeFailures, "include-failures", false, "include failed runs in analysis")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "verbose output (show fetch details)")

	return cmd
}

type analyzeOpts struct {
	repo            string
	branch          string
	since           string
	workflow        string
	format          string
	rawOutput       string
	noCache         bool
	includeFailures bool
	verbose         bool
}

func runAnalyze(cmd *cobra.Command, opts analyzeOpts) error {
	totalStart := time.Now()
	prog := output.NewProgress()
	prog.Log("Snitching on %s", opts.repo)

	sinceTime, err := parseSince(opts.since)
	if err != nil {
		return fmt.Errorf("invalid --since value: %w", err)
	}

	token, err := github.ResolveToken()
	if err != nil {
		return err
	}

	client, err := github.NewClient(token, opts.repo)
	if err != nil {
		return err
	}

	// Open store
	var s runStore
	if !opts.noCache {
		dbPath, err := store.DefaultPath()
		if err != nil {
			return err
		}
		st, err := store.Open(dbPath)
		if err != nil {
			return err
		}
		defer st.Close() //nolint:errcheck // error on deferred close has no actionable caller
		if opts.verbose {
			prog.Log("Cache: %s", dbPath)
		}
		s = st
	}

	result, err := fetchAndAnalyze(cmd.Context(), client, s, opts, sinceTime, prog)
	if err != nil {
		return err
	}

	for _, w := range result.Warnings {
		prog.Log("Analysis warning: %s", w.Message)
	}

	// Blank line before output
	_, _ = fmt.Fprintln(os.Stderr)

	// Output
	formatStart := time.Now()
	formatter, ok := output.Get(opts.format, output.Options{Verbose: opts.verbose, RawOutputPath: opts.rawOutput})
	if !ok {
		return fmt.Errorf("unknown format %q (supported: table, json, markdown, llm)", opts.format)
	}
	err = formatter.Format(cmd.OutOrStdout(), result)
	if opts.verbose {
		prog.Log("Format: %s", time.Since(formatStart))
	}
	prog.Log("Total: %s", time.Since(totalStart))
	return err
}

// fetchAndAnalyze contains the core pipeline: fetch workflows, hydrate runs, preprocess, analyze.
// Extracted from runAnalyze for testability — accepts interfaces instead of concrete types.
func fetchAndAnalyze(ctx context.Context, client workflowFetcher, s runStore, opts analyzeOpts, sinceTime time.Time, prog *output.Progress) (analyze.AnalysisResult, error) {
	// Fetch workflows
	prog.Status("Discovering workflows...")
	workflows, err := client.ListWorkflows(ctx)
	if err != nil {
		prog.Done()
		return analyze.AnalysisResult{}, fmt.Errorf("list workflows: %w", err)
	}
	if opts.verbose {
		prog.Log("Found %d workflows", len(workflows))
	}

	// Collect all run details (parallel across workflows)
	var (
		allDetails []model.RunDetail
		mu         sync.Mutex
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4) // max parallel workflows
	for _, wf := range workflows {
		if opts.workflow != "" && wf.Name != opts.workflow {
			continue
		}

		g.Go(func() error {
			prog.Status("Fetching %q...", wf.Name)
			fetchStart := time.Now()
			runs, fetchWarnings, err := client.FetchRuns(gctx, wf.ID, sinceTime, opts.branch)
			if err != nil {
				return fmt.Errorf("fetch runs for %q: %w", wf.Name, err)
			}
			for _, w := range fetchWarnings {
				prog.Log("WARNING: %s", w.Message)
			}
			if opts.verbose {
				prog.Log("  %q: fetched %d runs in %s", wf.Name, len(runs), time.Since(fetchStart))
			}

			// Partition runs: serve completed from cache, fetch only new/incomplete from API.
			var details []model.RunDetail
			var needsFetch []model.WorkflowRun

			if s != nil {
				cachedSet := make(map[int64]bool)
				cached, cacheErr := s.RunsSince(wf.ID, sinceTime)
				if cacheErr == nil {
					for _, r := range cached {
						cachedSet[r.ID] = true
					}
				}

				incompleteSet := make(map[int64]bool)
				incomplete, incErr := s.IncompleteRunIDs()
				if incErr == nil {
					for _, id := range incomplete {
						incompleteSet[id] = true
					}
				}

				for _, r := range runs {
					if cachedSet[r.ID] && !incompleteSet[r.ID] {
						d, loadErr := s.LoadRunDetail(r.ID)
						if loadErr == nil {
							details = append(details, *d)
							continue
						}
					}
					needsFetch = append(needsFetch, r)
				}

				if opts.verbose {
					prog.Log("  %q: %d cached, %d to fetch", wf.Name, len(details), len(needsFetch))
				}
			} else {
				needsFetch = runs
			}

			if len(needsFetch) > 0 {
				prog.Status("Fetching %q — hydrating %d runs (%d cached)...", wf.Name, len(needsFetch), len(details))
				hydrateStart := time.Now()
				fetched, warnings := client.FetchRunDetails(gctx, needsFetch)
				if opts.verbose {
					prog.Log("  %q: hydrated %d runs in %s", wf.Name, len(fetched), time.Since(hydrateStart))
				}
				for _, w := range warnings {
					prog.Log("WARNING: %s", w.Message)
				}

				if s != nil {
					if err := s.SaveRunDetails(fetched); err != nil {
						prog.Log("WARNING: failed to cache %q: %v", wf.Name, err)
					}
				}

				details = append(details, fetched...)
			}

			mu.Lock()
			allDetails = append(allDetails, details...)
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		prog.Done()
		return analyze.AnalysisResult{}, err
	}
	prog.Done()

	if len(allDetails) == 0 {
		return analyze.AnalysisResult{}, fmt.Errorf("no runs found for %s since %s", opts.repo, sinceTime.Format("2006-01-02"))
	}

	// Compute rerun stats before deduplication (needs to see all attempts)
	rerunStats := preprocess.ComputeRerunStats(allDetails)

	// Deduplicate retried runs for all downstream consumers.
	// allDetails may contain duplicate run IDs from overlapping API date windows.
	// This is separate from the dedup inside preprocess.Run — that one only applies
	// to its filtered output, but allDetails is passed directly to the engine.
	allDetails = preprocess.DeduplicateRetries(allDetails)

	// Preprocess: branch filter + failure exclusion (for duration analysis)
	ppStart := time.Now()
	filtered, ppWarnings := preprocess.Run(allDetails, preprocess.Options{
		Branch:          opts.branch,
		IncludeFailures: opts.includeFailures,
	})
	if opts.verbose {
		prog.Log("Preprocess: %s", time.Since(ppStart))
	}
	for _, w := range ppWarnings {
		if opts.verbose {
			prog.Log("Preprocessing: %s", w.Message)
		}
	}

	if len(filtered) == 0 {
		return analyze.AnalysisResult{}, fmt.Errorf("all %d runs were filtered out during preprocessing", len(allDetails))
	}

	prog.Status("Analyzing %d runs...", len(filtered))

	// Run analysis
	analyzeStart := time.Now()
	engine := analyze.NewEngine(analyze.DefaultAnalyzers()...)
	workflowNames := make(map[int64]string, len(workflows))
	for _, wf := range workflows {
		workflowNames[wf.ID] = wf.Name
	}
	result := engine.Run(ctx, filtered, allDetails, rerunStats, workflowNames)
	result.Meta.Repo = opts.repo
	prog.Done()
	if opts.verbose {
		prog.Log("Analyze: %s", time.Since(analyzeStart))
	}

	return result, nil
}

var gitHubRemoteRe = regexp.MustCompile(`github\.com[:/]([^/]+/[^/.]+?)(?:\.git)?$`)

// detectGitHubRepo extracts owner/repo from the git remote in the current directory.
func detectGitHubRepo() (string, error) {
	url, err := system.Run(context.Background(), "git", "remote", "get-url", "origin")
	if err != nil {
		return "", errors.New("not a git repository or no 'origin' remote")
	}
	m := gitHubRemoteRe.FindStringSubmatch(url)
	if m == nil {
		return "", fmt.Errorf("remote %q is not a GitHub repository", url)
	}
	return m[1], nil
}

func parseSince(s string) (time.Time, error) {
	return parseSinceFrom(s, time.Now().UTC())
}

var sinceRe = regexp.MustCompile(`^(\d+)(d|w|mo)$`)

func parseSinceFrom(s string, now time.Time) (time.Time, error) {
	// Try absolute date first
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}

	m := sinceRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("unrecognized format %q (use Nd, Nw, Nmo, or YYYY-MM-DD)", s)
	}

	n, _ := strconv.Atoi(m[1]) // regex guarantees digits
	switch m[2] {
	case "d":
		return now.AddDate(0, 0, -n), nil
	case "w":
		return now.AddDate(0, 0, -n*7), nil
	case "mo":
		return now.AddDate(0, -n, 0), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized format %q", s)
}
