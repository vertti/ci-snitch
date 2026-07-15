package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/store"
)

// doctorCheck is one environment validation. Informational checks report
// their outcome but never fail doctor (e.g. not being inside a git repo is a
// normal way to run the tool).
type doctorCheck struct {
	name          string
	informational bool
	run           func() (string, error)
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "doctor",
		Short:        "Validate the environment: token, API access, cache, git remote",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runChecks(cmd.OutOrStdout(), defaultChecks(cmd.Context()))
		},
	}
}

// runChecks executes every check (a failed one doesn't stop the report) and
// returns an error if any non-informational check failed.
func runChecks(w io.Writer, checks []doctorCheck) error {
	failed := 0
	for _, c := range checks {
		detail, err := c.run()
		switch {
		case err != nil && c.informational:
			_, _ = fmt.Fprintf(w, "note %s: %v\n", c.name, err)
		case err != nil:
			_, _ = fmt.Fprintf(w, "FAIL %s: %v\n", c.name, err)
			failed++
		default:
			_, _ = fmt.Fprintf(w, "ok   %s: %s\n", c.name, detail)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	return nil
}

func defaultChecks(ctx context.Context) []doctorCheck {
	var token string
	return []doctorCheck{
		{name: "token", run: func() (string, error) {
			var err error
			token, err = github.ResolveToken()
			if err != nil {
				return "", err
			}
			return "resolved", nil
		}},
		{name: "github api", run: func() (string, error) {
			if token == "" {
				return "", errors.New("skipped: no token")
			}
			c, err := github.NewClient(token, "example/example")
			if err != nil {
				return "", err
			}
			rlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			rl, err := c.RateLimit(rlCtx)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("reachable, core %d/%d, graphql %d/%d remaining",
				rl.Core.Remaining, rl.Core.Limit, rl.GraphQL.Remaining, rl.GraphQL.Limit), nil
		}},
		{name: "cache", run: func() (string, error) {
			path, err := store.DefaultPath()
			if err != nil {
				return "", err
			}
			st, err := store.Open(path)
			if err != nil {
				return "", fmt.Errorf("%s: %w", path, err)
			}
			_ = st.Close()
			return path, nil
		}},
		{name: "git remote", informational: true, run: func() (string, error) {
			repo, err := detectGitHubRepo()
			if err != nil {
				return "", fmt.Errorf("%w — pass owner/repo explicitly", err)
			}
			return repo, nil
		}},
	}
}
