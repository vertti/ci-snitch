package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/vertti/ci-snitch/internal/app"
	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/output"
	"github.com/vertti/ci-snitch/internal/store"
	"github.com/vertti/ci-snitch/internal/system"
)

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

			sinceTime, err := parseSince(since)
			if err != nil {
				return fmt.Errorf("invalid --since value: %w", err)
			}

			token, err := github.ResolveToken()
			if err != nil {
				return err
			}

			client, err := github.NewClient(token, repo)
			if err != nil {
				return err
			}

			prog := output.NewProgress()
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
			_, _ = fmt.Fprintln(os.Stderr)

			// Output
			formatStart := time.Now()
			formatter, ok := output.Get(format, output.Options{Verbose: verbose, RawOutputPath: rawOutput})
			if !ok {
				return fmt.Errorf("unknown format %q (supported: table, json, markdown, llm)", format)
			}
			err = formatter.Format(cmd.OutOrStdout(), &result)
			if verbose {
				prog.Log("Format: %s", time.Since(formatStart))
			}
			prog.Log("Total: %s", time.Since(totalStart))
			return err
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "filter to this branch (default: all branches)")
	cmd.Flags().StringVar(&since, "since", "30d", "how far back to analyze (e.g. 30d, 2w, 3mo, 2026-01-01)")
	cmd.Flags().StringVar(&workflow, "workflow", "", "filter to this workflow name")
	cmd.Flags().StringVar(&format, "format", "table", "output format: table, json, markdown, llm")
	cmd.Flags().StringVar(&rawOutput, "raw-output", "", "write full JSON to file (useful with --format llm to keep report compact)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "bypass local cache, fetch fresh data")
	cmd.Flags().BoolVar(&includeFailures, "include-failures", false, "include failed runs in analysis")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "verbose output (show fetch details)")

	return cmd
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
