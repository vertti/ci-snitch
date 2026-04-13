// Package preprocess provides filters and transformations for workflow run data
// that must be applied before any statistical analysis.
package preprocess

import (
	"fmt"

	"github.com/vertti/ci-snitch/internal/model"
)

// Warning represents a non-fatal issue found during preprocessing.
type Warning struct {
	Message string
}

// Options controls which preprocessing steps are applied.
type Options struct {
	Branch          string // filter to this branch (empty = no filter)
	IncludeFailures bool   // if false, exclude non-success conclusions
}

// Run applies all preprocessing steps in order and returns the filtered results.
func Run(details []model.RunDetail, opts Options) ([]model.RunDetail, []Warning) {
	var warnings []Warning

	result := DeduplicateRetries(details)
	if len(result) < len(details) {
		warnings = append(warnings, Warning{
			Message: fmt.Sprintf("deduplicated %d retried runs", len(details)-len(result)),
		})
	}

	if opts.Branch != "" {
		before := len(result)
		result = FilterByBranch(result, opts.Branch)
		if len(result) == 0 && before > 0 {
			warnings = append(warnings, Warning{
				Message: fmt.Sprintf("no runs found for branch %q (had %d runs on other branches)", opts.Branch, before),
			})
		}
	}

	if !opts.IncludeFailures {
		before := len(result)
		result = ExcludeFailures(result)
		if len(result) < before {
			warnings = append(warnings, Warning{
				Message: fmt.Sprintf("excluded %d non-success runs from duration analysis", before-len(result)),
			})
		}
	}

	return result, warnings
}

// FilterByBranch keeps only runs from the specified branch.
func FilterByBranch(details []model.RunDetail, branch string) []model.RunDetail {
	var out []model.RunDetail
	for _, d := range details {
		if d.Run.HeadBranch == branch {
			out = append(out, d)
		}
	}
	return out
}

// ExcludeFailures keeps only runs with conclusion "success".
func ExcludeFailures(details []model.RunDetail) []model.RunDetail {
	var out []model.RunDetail
	for _, d := range details {
		if d.Run.Conclusion == "success" {
			out = append(out, d)
		}
	}
	return out
}

// DeduplicateRetries keeps only the latest attempt for each run ID.
// GitHub Actions creates new run_attempt numbers for re-runs but keeps the same run ID.
func DeduplicateRetries(details []model.RunDetail) []model.RunDetail {
	best := make(map[int64]model.RunDetail)
	for _, d := range details {
		existing, ok := best[d.Run.ID]
		if !ok || d.Run.RunAttempt > existing.Run.RunAttempt {
			best[d.Run.ID] = d
		}
	}

	out := make([]model.RunDetail, 0, len(best))
	// Preserve original order
	seen := make(map[int64]bool)
	for _, d := range details {
		if !seen[d.Run.ID] {
			seen[d.Run.ID] = true
			out = append(out, best[d.Run.ID])
		}
	}
	return out
}

// RerunStats holds retry statistics for a workflow.
type RerunStats struct {
	UniqueRuns    int     // distinct run IDs
	RetriedRuns   int     // runs with more than 1 attempt
	ExtraAttempts int     // total extra attempts (sum of max_attempt - 1 per retried run)
	RerunRate     float64 // fraction of unique runs that were retried
}

// ComputeRerunStats computes per-workflow retry statistics from unfiltered data.
// Must be called before DeduplicateRetries.
// Returns only workflows that had at least one retry.
func ComputeRerunStats(details []model.RunDetail) map[string]RerunStats {
	// Per workflow, track max attempt per run ID.
	type wfRuns struct {
		maxAttempt map[int64]int
	}
	byWorkflow := make(map[string]*wfRuns)

	for _, d := range details {
		name := d.Run.WorkflowName
		if byWorkflow[name] == nil {
			byWorkflow[name] = &wfRuns{maxAttempt: make(map[int64]int)}
		}
		wr := byWorkflow[name]
		if d.Run.RunAttempt > wr.maxAttempt[d.Run.ID] {
			wr.maxAttempt[d.Run.ID] = d.Run.RunAttempt
		}
	}

	result := make(map[string]RerunStats)
	for name, wr := range byWorkflow {
		var s RerunStats
		s.UniqueRuns = len(wr.maxAttempt)
		for _, maxAttempt := range wr.maxAttempt {
			if maxAttempt > 1 {
				s.RetriedRuns++
				s.ExtraAttempts += maxAttempt - 1
			}
		}
		if s.RetriedRuns == 0 {
			continue
		}
		s.RerunRate = float64(s.RetriedRuns) / float64(s.UniqueRuns)
		result[name] = s
	}
	return result
}

// GroupMatrixJobs groups jobs by their base name (stripping matrix parameters).
// Matrix jobs appear as "test (ubuntu-latest, 20)" — this extracts "test" as the group key.
// Returns a map from group name to the list of run details (unchanged), plus a map
// from group name to the distinct matrix variants seen.
func GroupMatrixJobs(details []model.RunDetail) map[string][]string {
	variants := make(map[string]map[string]bool)
	for _, d := range details {
		for _, j := range d.Jobs {
			base, variant := ParseMatrixJobName(j.Name)
			if variants[base] == nil {
				variants[base] = make(map[string]bool)
			}
			if variant != "" {
				variants[base][variant] = true
			}
		}
	}

	result := make(map[string][]string, len(variants))
	for base, vs := range variants {
		keys := make([]string, 0, len(vs))
		for v := range vs {
			keys = append(keys, v)
		}
		result[base] = keys
	}
	return result
}

// ParseMatrixJobName splits a job name like "test (ubuntu-latest, 20)" into
// base="test" and variant="ubuntu-latest, 20".
// If there are no parentheses, variant is empty.
func ParseMatrixJobName(name string) (base, variant string) {
	for i, ch := range name {
		if ch == '(' {
			base = name[:i]
			// Trim trailing space from base
			for len(base) > 0 && base[len(base)-1] == ' ' {
				base = base[:len(base)-1]
			}
			// Extract variant (strip parens)
			variant = name[i+1:]
			if len(variant) > 0 && variant[len(variant)-1] == ')' {
				variant = variant[:len(variant)-1]
			}
			return base, variant
		}
	}
	return name, ""
}
