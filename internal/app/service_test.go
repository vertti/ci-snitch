package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/analyze"
	"github.com/vertti/ci-snitch/internal/diag"
	"github.com/vertti/ci-snitch/internal/github"
	"github.com/vertti/ci-snitch/internal/model"
	"github.com/vertti/ci-snitch/internal/output"
)

var testBase = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

type fakeFetcher struct {
	workflows       []model.Workflow
	runs            map[int64][]model.WorkflowRun
	listWarnings    []diag.Diagnostic
	details         []model.RunDetail
	hydrateWarnings []diag.Diagnostic

	mu          sync.Mutex
	hydratedIDs []int64 // run IDs requested via FetchRunDetails*()
}

func (f *fakeFetcher) ListWorkflows(context.Context) ([]model.Workflow, error) {
	return f.workflows, nil
}

func (f *fakeFetcher) FetchRuns(_ context.Context, workflowID int64, _ time.Time, _ string) ([]model.WorkflowRun, []diag.Diagnostic, error) {
	return f.runs[workflowID], f.listWarnings, nil
}

func (f *fakeFetcher) FetchRunDetails(ctx context.Context, runs []model.WorkflowRun) ([]model.RunDetail, []diag.Diagnostic) {
	return f.FetchRunDetailsGraphQL(ctx, runs)
}

// FetchRunDetailsGraphQL records the requested run IDs and returns the
// configured details for exactly those runs.
func (f *fakeFetcher) FetchRunDetailsGraphQL(_ context.Context, runs []model.WorkflowRun) ([]model.RunDetail, []diag.Diagnostic) {
	requested := make(map[int64]bool, len(runs))
	f.mu.Lock()
	for i := range runs {
		f.hydratedIDs = append(f.hydratedIDs, runs[i].ID)
		requested[runs[i].ID] = true
	}
	f.mu.Unlock()

	var out []model.RunDetail
	for i := range f.details {
		if requested[f.details[i].Run.ID] {
			out = append(out, f.details[i])
		}
	}
	return out, f.hydrateWarnings
}

func (f *fakeFetcher) RateLimit(context.Context) (github.RateLimitStatus, error) {
	return github.RateLimitStatus{Remaining: 5000, Limit: 5000, ResetAt: testBase.Add(time.Hour)}, nil
}

type fakeStore struct {
	saveErr       error
	cachedRuns    []model.WorkflowRun
	cachedDetails map[int64]*model.RunDetail
}

func (s *fakeStore) RunsSince(int64, time.Time) ([]model.WorkflowRun, error) {
	return s.cachedRuns, nil
}
func (s *fakeStore) IncompleteRunIDs() ([]int64, error) { return nil, nil }
func (s *fakeStore) LoadRunDetail(id int64) (*model.RunDetail, error) {
	if d, ok := s.cachedDetails[id]; ok {
		return d, nil
	}
	return nil, errors.New("not cached")
}
func (s *fakeStore) SaveRunDetails([]model.RunDetail) error { return s.saveErr }

func fakeRunDetail(id int64, conclusion string) model.RunDetail {
	created := testBase.Add(time.Duration(id) * time.Minute)
	return model.RunDetail{
		Run: model.WorkflowRun{
			ID: id, WorkflowID: 1, WorkflowName: "CI", Name: "push",
			Event: "push", Status: "completed", Conclusion: conclusion,
			HeadBranch: "main", HeadSHA: "abc123", RunAttempt: 1,
			CreatedAt: created, StartedAt: created.Add(5 * time.Second),
			UpdatedAt: created.Add(3 * time.Minute),
		},
		Jobs: []model.Job{{
			ID: 1000 + id, RunID: id, Name: "build", Status: "completed", Conclusion: conclusion,
			StartedAt: created.Add(10 * time.Second), CompletedAt: created.Add(2 * time.Minute),
			Labels: []string{"ubuntu-latest"},
		}},
	}
}

func baseFetcher() *fakeFetcher {
	details := []model.RunDetail{fakeRunDetail(1, "success"), fakeRunDetail(2, "success")}
	runs := make([]model.WorkflowRun, len(details))
	for i := range details {
		runs[i] = details[i].Run
	}
	return &fakeFetcher{
		workflows: []model.Workflow{{ID: 1, Name: "CI"}},
		runs:      map[int64][]model.WorkflowRun{1: runs},
		details:   details,
	}
}

func runService(t *testing.T, f *fakeFetcher, store RunStore) (analyze.AnalysisResult, error) {
	t.Helper()
	svc := &Service{Client: f, Store: store, Prog: output.NewProgress()}
	return svc.Run(context.Background(), &Options{
		Repo:  "example-org/example-repo",
		Since: testBase.Add(-24 * time.Hour),
	})
}

func assertHasDiagnostic(t *testing.T, diags []diag.Diagnostic, kind diag.Kind, substr string) {
	t.Helper()
	for _, d := range diags {
		if d.Kind == kind && strings.Contains(d.Message, substr) {
			return
		}
	}
	t.Errorf("no %s diagnostic containing %q in %v", kind, substr, diags)
}

func TestRun_ListWarningsSurfaceInDiagnostics(t *testing.T) {
	f := baseFetcher()
	f.listWarnings = []diag.Diagnostic{diag.New(
		diag.Warn, diag.KindPartialData, "workflow-1",
		"has 1500 runs in window, results may be truncated (GitHub API cap is 1000)",
	)}

	res, err := runService(t, f, nil)
	require.NoError(t, err)
	assertHasDiagnostic(t, res.Diagnostics, diag.KindPartialData, "truncated")
}

func TestRun_HydrationWarningsSurfaceInDiagnostics(t *testing.T) {
	f := baseFetcher()
	f.hydrateWarnings = []diag.Diagnostic{diag.New(
		diag.Warn, diag.KindNetwork, "run-2",
		"failed to fetch run 2: boom",
	)}

	res, err := runService(t, f, nil)
	require.NoError(t, err)
	assertHasDiagnostic(t, res.Diagnostics, diag.KindNetwork, "failed to fetch run 2")
}

func TestRun_PreprocessWarningsSurfaceInDiagnostics(t *testing.T) {
	f := baseFetcher()
	// One failed run: with IncludeFailures=false preprocess excludes it and warns.
	f.details = append(f.details, fakeRunDetail(3, "failure"))
	f.runs[1] = append(f.runs[1], f.details[2].Run)

	res, err := runService(t, f, nil)
	require.NoError(t, err)
	assertHasDiagnostic(t, res.Diagnostics, diag.KindPreprocess, "excluded 1 non-success runs")
}

func TestRun_BranchFilterAppliesToFailureAnalysis(t *testing.T) {
	// 6 successes on main, 4 failures on a feature branch. With --branch main
	// the failure analyzer must not see the feature-branch failures.
	var details []model.RunDetail
	for i := int64(1); i <= 6; i++ {
		details = append(details, fakeRunDetail(i, "success"))
	}
	for i := int64(7); i <= 10; i++ {
		d := fakeRunDetail(i, "failure")
		d.Run.HeadBranch = "feature"
		details = append(details, d)
	}
	runs := make([]model.WorkflowRun, len(details))
	for i := range details {
		runs[i] = details[i].Run
	}
	f := &fakeFetcher{
		workflows: []model.Workflow{{ID: 1, Name: "CI"}},
		runs:      map[int64][]model.WorkflowRun{1: runs},
		details:   details,
	}

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	res, err := svc.Run(context.Background(), &Options{
		Repo:   "example-org/example-repo",
		Since:  testBase.Add(-24 * time.Hour),
		Branch: "main",
	})
	require.NoError(t, err)

	for _, finding := range res.Findings {
		if finding.Type == analyze.TypeFailure {
			t.Errorf("failure finding %q leaked from a filtered-out branch: %s",
				finding.Title, finding.Description)
		}
	}
}

func TestRun_BranchWithNoRuns_ErrorNamesTheBranch(t *testing.T) {
	f := baseFetcher() // all runs on main

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	_, err := svc.Run(context.Background(), &Options{
		Repo:   "example-org/example-repo",
		Since:  testBase.Add(-24 * time.Hour),
		Branch: "nope",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), `branch "nope"`,
		"error must name the branch filter instead of a generic preprocessing message")
}

func TestRun_StaleCachedRunIsRefetched(t *testing.T) {
	// Run 1 was cached at attempt 1; it has since been re-run (newer
	// UpdatedAt, attempt 2). The listing's fresh metadata must win over
	// bare ID membership in the cache.
	f := baseFetcher()
	stale := f.details[0]
	stale.Run.UpdatedAt = stale.Run.UpdatedAt.Add(-time.Hour) // cached copy is older
	stale.Run.Conclusion = "failure"

	st := &fakeStore{
		cachedRuns:    []model.WorkflowRun{stale.Run},
		cachedDetails: map[int64]*model.RunDetail{stale.Run.ID: &stale},
	}

	_, err := runService(t, f, st)
	require.NoError(t, err)

	f.mu.Lock()
	defer f.mu.Unlock()
	require.True(t, slices.Contains(f.hydratedIDs, stale.Run.ID),
		"run with newer UpdatedAt in the listing must be re-fetched, got hydrated IDs %v", f.hydratedIDs)
}

func TestRun_FreshCachedRunServedFromCache(t *testing.T) {
	// Cached copy has the same UpdatedAt as the listing: serve from cache,
	// no hydration call for it.
	f := baseFetcher()
	cached := f.details[0]

	st := &fakeStore{
		cachedRuns:    []model.WorkflowRun{cached.Run},
		cachedDetails: map[int64]*model.RunDetail{cached.Run.ID: &cached},
	}

	_, err := runService(t, f, st)
	require.NoError(t, err)

	f.mu.Lock()
	defer f.mu.Unlock()
	require.False(t, slices.Contains(f.hydratedIDs, cached.Run.ID),
		"up-to-date cached run must not be re-fetched, got hydrated IDs %v", f.hydratedIDs)
}

func TestCountRuns_CountsStaleRunsAsUncached(t *testing.T) {
	fresh := fakeRunDetail(1, "success").Run
	staleCached := fakeRunDetail(2, "failure").Run
	staleListed := staleCached
	staleListed.UpdatedAt = staleListed.UpdatedAt.Add(time.Hour) // re-run since caching
	staleListed.RunAttempt = 2

	st := &fakeStore{cachedRuns: []model.WorkflowRun{fresh, staleCached}}
	svc := &Service{Store: st, Prog: output.NewProgress()}

	total, uncached := svc.countRuns([]workflowRuns{
		{wf: model.Workflow{ID: 1, Name: "CI"}, runs: []model.WorkflowRun{fresh, staleListed}},
	}, &Options{Since: testBase.Add(-24 * time.Hour)})

	require.Equal(t, 2, total)
	require.Equal(t, 1, uncached, "the re-run run must be counted as needing a fetch")
}

func TestRun_CacheSaveFailureSurfacesInDiagnostics(t *testing.T) {
	f := baseFetcher()
	st := &fakeStore{saveErr: errors.New("disk full")}

	res, err := runService(t, f, st)
	require.NoError(t, err)
	assertHasDiagnostic(t, res.Diagnostics, diag.KindCache, "failed to cache")
}
