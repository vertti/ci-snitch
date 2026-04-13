package main

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/vertti/ci-snitch/internal/analyze"
	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/output"
	"github.com/vertti/ci-snitch/internal/preprocess"
	"github.com/vertti/ci-snitch/internal/store"
)

func newAnalyzeCmd() *cobra.Command {
	var (
		repo            string
		branch          string
		since           string
		workflow        string
		format          string
		noCache         bool
		includeFailures bool
		verbose         bool
	)

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze CI workflow performance",
		Long:  "Fetch workflow run data and compute performance statistics, outliers, and trends.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAnalyze(cmd, analyzeOpts{
				repo:            repo,
				branch:          branch,
				since:           since,
				workflow:        workflow,
				format:          format,
				noCache:         noCache,
				includeFailures: includeFailures,
				verbose:         verbose,
			})
		},
	}

	cmd.Flags().StringVar(&repo, "repo", "", "repository in owner/repo format (required)")
	cmd.Flags().StringVar(&branch, "branch", "", "filter to this branch (default: all branches)")
	cmd.Flags().StringVar(&since, "since", "60d", "how far back to analyze (e.g. 60d, 2026-01-01)")
	cmd.Flags().StringVar(&workflow, "workflow", "", "filter to this workflow name")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json, markdown")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "bypass local cache, fetch fresh data")
	cmd.Flags().BoolVar(&includeFailures, "include-failures", false, "include failed runs in analysis")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "verbose output (show fetch details)")
	_ = cmd.MarkFlagRequired("repo")

	return cmd
}

type analyzeOpts struct {
	repo            string
	branch          string
	since           string
	workflow        string
	format          string
	noCache         bool
	includeFailures bool
	verbose         bool
}

func runAnalyze(cmd *cobra.Command, opts analyzeOpts) error {
	totalStart := time.Now()
	prog := output.NewProgress()

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
	var s *store.Store
	if !opts.noCache {
		dbPath, err := store.DefaultPath()
		if err != nil {
			return err
		}
		s, err = store.Open(dbPath)
		if err != nil {
			return err
		}
		defer s.Close() //nolint:errcheck // error on deferred close has no actionable caller
		if opts.verbose {
			prog.Log("Cache: %s", dbPath)
		}
	}

	ctx := cmd.Context()

	// Fetch workflows
	prog.Status("Discovering workflows...")
	workflows, err := client.ListWorkflows(ctx)
	if err != nil {
		prog.Done()
		return fmt.Errorf("list workflows: %w", err)
	}
	if opts.verbose {
		prog.Log("Found %d workflows", len(workflows))
	}

	// Collect all run details (parallel across workflows)
	var (
		allDetails []model.RunDetail
		mu         sync.Mutex
		sem        = make(chan struct{}, 4) // max parallel workflows
	)
	fetchedWorkflows := 0

	var wg sync.WaitGroup
	for _, wf := range workflows {
		if opts.workflow != "" && wf.Name != opts.workflow {
			continue
		}
		fetchedWorkflows++

		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			prog.Status("Fetching %q...", wf.Name)
			fetchStart := time.Now()
			runs, err := client.FetchRuns(ctx, wf.ID, sinceTime, opts.branch)
			if err != nil {
				prog.Log("WARNING: failed to fetch runs for %q: %v", wf.Name, err)
				return
			}
			if opts.verbose {
				prog.Log("  %q: fetched %d runs in %s", wf.Name, len(runs), time.Since(fetchStart))
			}

			prog.Status("Fetching %q — hydrating %d runs...", wf.Name, len(runs))
			hydrateStart := time.Now()
			details, warnings := client.FetchRunDetails(ctx, runs)
			if opts.verbose {
				prog.Log("  %q: hydrated %d runs in %s", wf.Name, len(details), time.Since(hydrateStart))
			}
			for _, w := range warnings {
				prog.Log("WARNING: %s", w.Message)
			}

			if s != nil {
				saveStart := time.Now()
				if err := s.SaveRunDetails(details); err != nil {
					prog.Log("WARNING: failed to cache %q: %v", wf.Name, err)
				}
				if opts.verbose {
					prog.Log("  %q: saved to cache in %s", wf.Name, time.Since(saveStart))
				}
			}

			mu.Lock()
			allDetails = append(allDetails, details...)
			mu.Unlock()
		})
	}
	wg.Wait()
	prog.Done()

	if len(allDetails) == 0 {
		return fmt.Errorf("no runs found for %s since %s", opts.repo, sinceTime.Format("2006-01-02"))
	}

	// Compute rerun stats before deduplication
	rerunStats := preprocess.ComputeRerunStats(allDetails)

	// Preprocess
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
		return fmt.Errorf("all %d runs were filtered out during preprocessing", len(allDetails))
	}

	prog.Status("Analyzing %d runs...", len(filtered))

	// Run analysis
	analyzeStart := time.Now()
	engine := analyze.NewEngine(
		analyze.SummaryAnalyzer{},
		analyze.OutlierAnalyzer{},
		analyze.ChangePointAnalyzer{},
		analyze.FailureAnalyzer{},
	)
	result := engine.Run(ctx, filtered, allDetails, rerunStats)
	prog.Done()
	if opts.verbose {
		prog.Log("Analyze: %s", time.Since(analyzeStart))
	}

	for _, w := range result.Warnings {
		prog.Log("Analysis warning: %s", w.Message)
	}

	// Blank line before output
	_, _ = fmt.Fprintln(os.Stderr)

	// Output
	formatStart := time.Now()
	formatter, ok := output.Get(opts.format, output.Options{Verbose: opts.verbose})
	if !ok {
		return fmt.Errorf("unknown format %q (supported: table, json, markdown)", opts.format)
	}
	err = formatter.Format(cmd.OutOrStdout(), result)
	if opts.verbose {
		prog.Log("Format: %s", time.Since(formatStart))
	}
	prog.Log("Total: %s", time.Since(totalStart))
	return err
}

func parseSince(s string) (time.Time, error) {
	// Try absolute date first
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}

	// Try relative duration (e.g. "60d", "2w", "3mo")
	if len(s) < 2 {
		return time.Time{}, fmt.Errorf("unrecognized format %q", s)
	}

	now := time.Now().UTC()
	suffix := s[len(s)-1]
	numStr := s[:len(s)-1]

	// Handle "mo" suffix
	if len(s) >= 3 && s[len(s)-2:] == "mo" {
		numStr = s[:len(s)-2]
		var n int
		if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil {
			return time.Time{}, fmt.Errorf("unrecognized format %q", s)
		}
		return now.AddDate(0, -n, 0), nil
	}

	var n int
	if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil {
		return time.Time{}, fmt.Errorf("unrecognized format %q", s)
	}

	switch suffix {
	case 'd':
		return now.AddDate(0, 0, -n), nil
	case 'w':
		return now.AddDate(0, 0, -n*7), nil
	default:
		return time.Time{}, fmt.Errorf("unrecognized suffix %q in %q (use d, w, or mo)", string(suffix), s)
	}
}
