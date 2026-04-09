// Package main is a manual smoke test that exercises the GitHub client against a real repo.
// Usage: go run ./cmd/smoke [owner/repo]
// Defaults to cli/cli if no repo is specified.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/vertti/ci-snitch/internal/github"
)

func main() {
	repo := "cli/cli"
	if len(os.Args) > 1 {
		repo = os.Args[1]
	}

	token, err := github.ResolveToken()
	if err != nil {
		log.Fatal(err)
	}

	c, err := github.NewClient(token, repo)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	workflows, err := c.ListWorkflows(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Found %d workflows\n", len(workflows))

	if len(workflows) == 0 {
		return
	}

	wf := workflows[0]
	since := time.Now().AddDate(0, 0, -3)
	runs, err := c.FetchRuns(ctx, wf.ID, since, "")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Workflow %q: %d runs in last 3 days\n", wf.Name, len(runs))

	limit := 3
	if len(runs) < limit {
		limit = len(runs)
	}
	if limit == 0 {
		fmt.Println("No runs to hydrate")
		return
	}

	details, warnings := c.FetchRunDetails(ctx, runs[:limit])
	fmt.Printf("Hydrated %d runs, %d warnings\n", len(details), len(warnings))

	for _, w := range warnings {
		fmt.Printf("  WARNING: %s: %v\n", w.Message, w.Err)
	}

	for _, d := range details {
		fmt.Printf("\nRun %d [%s] %s\n", d.Run.ID, d.Run.Conclusion, d.Run.Name)
		for _, j := range d.Jobs {
			fmt.Printf("  Job %q: %s (%d steps)\n", j.Name, j.Duration(), len(j.Steps))
			for _, s := range j.Steps {
				fmt.Printf("    Step %q: %s\n", s.Name, s.Duration())
			}
		}
	}
}
