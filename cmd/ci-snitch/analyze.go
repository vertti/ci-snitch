package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/vertti/ci-snitch/internal/analyze"
	"github.com/vertti/ci-snitch/internal/app"
	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/output"
	"github.com/vertti/ci-snitch/internal/store"
	"github.com/vertti/ci-snitch/internal/system"
)

func newAnalyzeCmd() *cobra.Command {
	var (
		branch          string
		branchCategory  string
		since           string
		workflow        string
		format          string
		rawOutput       string
		noCache         bool
		includeFailures bool
		verbose         bool
		quiet           bool
		failOn          string
	)

	cmd := &cobra.Command{
		Use:   "analyze [owner/repo]",
		Short: "Analyze CI workflow performance",
		Long: `Fetch workflow run data and compute performance statistics, outliers, and trends.

If no repository is specified, detects the GitHub remote from the current directory.`,
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate output options before anything that costs a network call.
			formatter, failConds, err := validateOutputOptions(format, rawOutput, failOn, branchCategory, verbose)
			if err != nil {
				return err
			}

			var repo string
			if len(args) > 0 {
				repo = args[0]
			} else {
				detected, err := detectGitHubRepo()
				if err != nil {
					return fmt.Errorf("%w\nprovide a repository: ci-snitch analyze <owner/repo>\nor run from inside a GitHub repo directory", err)
				}
				repo = detected
			}

			sinceTime, err := parseSince(since)
			if err != nil {
				return fmt.Errorf("invalid --since value: %w", err)
			}

			token, err := github.ResolveToken()
			if err != nil {
				return err
			}

			// The logger surfaces rare client events (rate-limit sleeps,
			// REST fallbacks) that would otherwise be silent hangs.
			clientLog := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			client, err := github.NewClient(token, repo, github.WithLogger(clientLog))
			if err != nil {
				return err
			}

			prog := output.NewProgress()
			if quiet {
				prog = output.NewProgressQuiet()
			}
			prog.Log("Snitching on %s", repo)

			// Open store
			var s app.RunStore
			if !noCache {
				dbPath, err := store.DefaultPath()
				if err != nil {
					return err
				}
				st, err := store.Open(dbPath)
				if err != nil {
					return err
				}
				defer st.Close() //nolint:errcheck // error on deferred close has no actionable caller
				if verbose {
					prog.Log("Cache: %s", dbPath)
				}
				s = st
			}

			totalStart := time.Now()
			svc := &app.Service{
				Client: client,
				Store:  s,
				Prog:   prog,
			}

			result, err := svc.Run(cmd.Context(), &app.Options{
				Repo:            repo,
				Branch:          branch,
				BranchCategory:  branchCategory,
				Since:           sinceTime,
				Workflow:        workflow,
				IncludeFailures: includeFailures,
				Verbose:         verbose,
			})
			if err != nil {
				return err
			}

			for _, d := range result.Diagnostics {
				prog.Log("%s", d)
			}

			// Blank line before output
			if !quiet {
				_, _ = fmt.Fprintln(os.Stderr)
			}

			// Output
			formatStart := time.Now()
			err = formatter.Format(cmd.OutOrStdout(), &result)
			if verbose {
				prog.Log("Format: %s", time.Since(formatStart))
			}
			prog.Log("Total: %s", time.Since(totalStart))
			if err != nil {
				return err
			}

			return applyFailOnGate(failConds, &result)
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "filter to this branch (default: all branches)")
	cmd.Flags().StringVar(&branchCategory, "branch-category", "", "filter by trigger: pr (pull_request runs), main (everything else), all")
	cmd.Flags().StringVar(&since, "since", "30d", "how far back to analyze (e.g. 30d, 2w, 3mo, 2026-01-01)")
	cmd.Flags().StringVar(&workflow, "workflow", "", "filter to this workflow name")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json, markdown, llm")
	cmd.Flags().StringVar(&rawOutput, "raw-output", "", "write full JSON to file (useful with --format llm to keep report compact)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "bypass local cache, fetch fresh data")
	cmd.Flags().BoolVar(&includeFailures, "include-failures", false, "include failed runs in analysis")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "verbose output (show fetch details)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress progress and diagnostic output on stderr")
	cmd.Flags().StringVar(&failOn, "fail-on", "", "exit 2 when conditions match: regression, failure-rate>N (comma-separated)")

	_ = cmd.RegisterFlagCompletionFunc("format", cobra.FixedCompletions(
		[]string{"table", "json", "markdown", "llm"}, cobra.ShellCompDirectiveNoFileComp))

	return cmd
}

// validateOutputOptions checks every flag that can be rejected without a
// network call and returns the pre-built formatter and fail-on conditions.
func validateOutputOptions(format, rawOutput, failOn, branchCategory string, verbose bool) (output.Formatter, []failOnCondition, error) {
	formatter, ok := output.Get(format, output.Options{Verbose: verbose, RawOutputPath: rawOutput})
	if !ok {
		return nil, nil, fmt.Errorf("unknown format %q (supported: table, json, markdown, llm)", format)
	}
	if rawOutput != "" && format != "llm" {
		return nil, nil, errors.New("--raw-output requires --format llm")
	}
	failConds, err := parseFailOn(failOn)
	if err != nil {
		return nil, nil, err
	}
	if err := validateBranchCategory(branchCategory); err != nil {
		return nil, nil, err
	}
	return formatter, failConds, nil
}

func validateBranchCategory(c string) error {
	switch c {
	case "", "all", "pr", "main":
		return nil
	default:
		return fmt.Errorf("invalid --branch-category %q (supported: pr, main, all)", c)
	}
}

var gitHubRemoteRe = regexp.MustCompile(`github\.com[:/]([^/]+/[^/]+?)(?:\.git)?$`)

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
		return t, validateSincePast(t, now)
	}

	m := sinceRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("unrecognized format %q (use Nd, Nw, Nmo, or YYYY-MM-DD)", s)
	}

	n, _ := strconv.Atoi(m[1]) // regex guarantees digits
	var t time.Time
	switch m[2] {
	case "d":
		t = now.AddDate(0, 0, -n)
	case "w":
		t = now.AddDate(0, 0, -n*7)
	case "mo":
		t = now.AddDate(0, -n, 0)
	default:
		return time.Time{}, fmt.Errorf("unrecognized format %q", s)
	}
	if err := validateSincePast(t, now); err != nil {
		return time.Time{}, err
	}
	// Truncate to UTC midnight: GitHub's created filter is date-only, so a
	// mid-day timestamp disagrees with what the API actually returns — runs
	// from the since-day's morning were listed but never matched the cache's
	// timestamp comparison, so they were re-fetched on every invocation.
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
}

// validateSincePast rejects windows that cannot contain any runs ("0d",
// future dates) before they burn an API round trip.
func validateSincePast(t, now time.Time) error {
	if !t.Before(now) {
		return fmt.Errorf("--since must be in the past, got %s", t.Format("2006-01-02"))
	}
	return nil
}

// applyFailOnGate prints tripped-condition reasons to stderr (even in quiet
// mode — the whole point of --fail-on is telling CI why the build failed) and
// returns exit code 2 via exitCodeError.
func applyFailOnGate(conds []failOnCondition, result *analyze.AnalysisResult) error {
	reasons := evaluateFailOn(conds, result)
	if len(reasons) == 0 {
		return nil
	}
	for _, r := range reasons {
		_, _ = fmt.Fprintf(os.Stderr, "fail-on: %s\n", r)
	}
	return &exitCodeError{code: 2, msg: fmt.Sprintf("%d --fail-on condition(s) tripped", len(reasons))}
}
