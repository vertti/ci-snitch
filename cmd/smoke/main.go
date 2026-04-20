// Package main is a manual smoke test that exercises the GitHub client and store against a real repo.
// Usage: go run ./cmd/smoke [owner/repo]
// Defaults to cli/cli if no repo is specified.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	repo := "cli/cli"
	if len(os.Args) > 1 {
		repo = os.Args[1]
	}

	token, err := github.ResolveToken()
	if err != nil {
		return err
	}

	c, err := github.NewClient(token, repo)
	if err != nil {
		return err
	}

	dbPath := filepath.Join(os.TempDir(), "ci-snitch-smoke.db")
	s, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // error on deferred close has no actionable caller
	fmt.Printf("Store: %s\n", dbPath)

	ctx := context.Background()

	workflows, err := c.ListWorkflows(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Found %d workflows\n", len(workflows))

	if len(workflows) == 0 {
		return nil
	}

	wf := workflows[0]
	since := time.Now().AddDate(0, 0, -3)
	runs, fetchWarnings, err := c.FetchRuns(ctx, wf.ID, since, "")
	if err != nil {
		return err
	}
	for _, w := range fetchWarnings {
		fmt.Printf("WARNING: %s\n", w.Message)
	}
	fmt.Printf("Workflow %q: %d runs in last 3 days\n", wf.Name, len(runs))

	limit := min(3, len(runs))
	if limit == 0 {
		fmt.Println("No runs to hydrate")
		return nil
	}

	details, warnings := c.FetchRunDetails(ctx, runs[:limit])
	fmt.Printf("Hydrated %d runs, %d warnings\n", len(details), len(warnings))

	for _, w := range warnings {
		fmt.Printf("  WARNING: %s: %v\n", w.Message, w.Err)
	}

	// Save to store
	if err := s.SaveRunDetails(details); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	fmt.Printf("Saved %d run details to store\n", len(details))

	// Load back and verify
	loaded, err := s.LoadRunDetails(wf.ID, since)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	fmt.Printf("Loaded %d run details from store\n", len(loaded))

	for i := range loaded {
		fmt.Printf("\nRun %d [%s] %s\n", loaded[i].Run.ID, loaded[i].Run.Conclusion, loaded[i].Run.Name)
		for j := range loaded[i].Jobs {
			fmt.Printf("  Job %q: %s (%d steps)\n", loaded[i].Jobs[j].Name, loaded[i].Jobs[j].Duration(), len(loaded[i].Jobs[j].Steps))
			for st := range loaded[i].Jobs[j].Steps {
				fmt.Printf("    Step %q: %s\n", loaded[i].Jobs[j].Steps[st].Name, loaded[i].Jobs[j].Steps[st].Duration())
			}
		}
	}

	// Check incomplete run tracking
	incomplete, err := s.IncompleteRunIDs()
	if err != nil {
		return fmt.Errorf("check incomplete: %w", err)
	}
	fmt.Printf("\nIncomplete runs in store: %d\n", len(incomplete))

	return nil
}
