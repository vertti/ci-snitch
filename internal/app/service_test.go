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
	listWorkflowErr error
	runs            map[int64][]model.WorkflowRun
	listWarnings    []diag.Diagnostic
	details         []model.RunDetail
	hydrateWarnings []diag.Diagnostic
	coreRemaining   int // 0 means "plenty" (5000)
	gqlRemaining    int // 0 means "plenty" (5000)
	rateErr         error
	// cancelDuringHydration simulates Ctrl+C arriving mid-hydration.
	cancelDuringHydration context.CancelFunc

	mu          sync.Mutex
	hydratedIDs []int64 // run IDs requested via FetchRunDetails*()
}

func (f *fakeFetcher) ListWorkflows(context.Context) ([]model.Workflow, error) {
	return f.workflows, f.listWorkflowErr
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
	if f.cancelDuringHydration != nil {
		f.cancelDuringHydration()
	}
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
	if f.rateErr != nil {
		return github.RateLimitStatus{}, f.rateErr
	}
	pool := func(remaining int) github.RatePool {
		if remaining == 0 {
			remaining = 5000
		}
		return github.RatePool{Remaining: remaining, Limit: 5000, ResetAt: testBase.Add(time.Hour)}
	}
	return github.RateLimitStatus{
		Core:    pool(f.coreRemaining),
		GraphQL: pool(f.gqlRemaining),
	}, nil
}

type fakeStore struct {
	saveErr       error
	cachedRuns    []model.WorkflowRun
	cachedDetails map[int64]*model.RunDetail

	mu    sync.Mutex
	saved []model.RunDetail
}

func (s *fakeStore) RunsSince(int64, time.Time) ([]model.WorkflowRun, error) {
	return s.cachedRuns, nil
}
func (s *fakeStore) IncompleteRunIDs() ([]int64, error) { return nil, nil }
func (s *fakeStore) LoadRunDetailsByIDs(ids []int64) ([]model.RunDetail, error) {
	var out []model.RunDetail
	for _, id := range ids {
		if d, ok := s.cachedDetails[id]; ok {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (s *fakeStore) SaveRunDetails(details []model.RunDetail) error {
	s.mu.Lock()
	s.saved = append(s.saved, details...)
	s.mu.Unlock()
	return s.saveErr
}

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

// manyRunsFetcher builds a fetcher with n success runs for workflow 1.
func manyRunsFetcher(n int) *fakeFetcher {
	details := make([]model.RunDetail, n)
	runs := make([]model.WorkflowRun, n)
	for i := range n {
		details[i] = fakeRunDetail(int64(i+1), "success")
		runs[i] = details[i].Run
	}
	return &fakeFetcher{
		workflows: []model.Workflow{{ID: 1, Name: "CI"}},
		runs:      map[int64][]model.WorkflowRun{1: runs},
		details:   details,
	}
}

func TestRun_RateBudgetAbortsBeforeHydration(t *testing.T) {
	f := manyRunsFetcher(25) // ~2 estimated GraphQL calls
	f.gqlRemaining = 1       // hydration is GraphQL: budget floor 0.8 * 1 = 0 allowed calls

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	_, err := svc.Run(context.Background(), &Options{
		Repo:    "example-org/example-repo",
		Since:   testBase.Add(-24 * time.Hour),
		Verbose: true, // also exercises the budget log line
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "rate limit budget")
	require.Contains(t, err.Error(), "--since 7d", "abort message must be actionable")

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Empty(t, f.hydratedIDs, "no hydration calls may be spent after a budget abort")
}

func TestRun_BudgetChecksGraphQLPoolNotCore(t *testing.T) {
	// Hydration spends GraphQL points; a depleted core pool (used only for
	// listing, which has already happened) must not abort the run.
	f := manyRunsFetcher(25)
	f.coreRemaining = 1

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	_, err := svc.Run(context.Background(), &Options{
		Repo:  "example-org/example-repo",
		Since: testBase.Add(-24 * time.Hour),
	})
	require.NoError(t, err, "low core pool must not abort GraphQL hydration")
}

func TestRun_BudgetAbortNamesGraphQLPool(t *testing.T) {
	f := manyRunsFetcher(25)
	f.gqlRemaining = 1

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	_, err := svc.Run(context.Background(), &Options{
		Repo:  "example-org/example-repo",
		Since: testBase.Add(-24 * time.Hour),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "GraphQL", "abort message must name the exhausted pool")
}

func TestRun_RateLimitReadErrorIsNonFatal(t *testing.T) {
	f := baseFetcher()
	f.rateErr = errors.New("rate limit endpoint down")

	_, err := runService(t, f, nil)
	require.NoError(t, err, "an unreadable rate limit must not block analysis")
}

func TestRun_AllRunsFilteredOutError(t *testing.T) {
	f := baseFetcher()
	for i := range f.details {
		f.details[i].Run.Conclusion = "failure" // IncludeFailures defaults to false
	}
	for i, r := range f.runs[1] {
		r.Conclusion = "failure"
		f.runs[1][i] = r
	}

	_, err := runService(t, f, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "filtered out during preprocessing")
}

func TestRun_CacheLoadErrorFallsBackToFetch(t *testing.T) {
	// The run is listed as cached but its detail row cannot be loaded:
	// hydration must fetch it rather than dropping it.
	f := baseFetcher()
	st := &fakeStore{
		cachedRuns:    []model.WorkflowRun{f.details[0].Run},
		cachedDetails: nil, // LoadRunDetail errors for every ID
	}

	res, err := runService(t, f, st)
	require.NoError(t, err)
	require.Equal(t, 2, res.Meta.TotalRuns)

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Contains(t, f.hydratedIDs, f.details[0].Run.ID,
		"a cached-but-unloadable run must be re-fetched")
}

func TestRun_RerunStatsReachFailureDetail(t *testing.T) {
	// Six unique runs: four successes, one genuine failure (so a failure
	// finding exists), and one run with two attempts. The failure detail
	// must carry a non-zero rerun rate computed from all attempts.
	var details []model.RunDetail
	for i := int64(1); i <= 4; i++ {
		details = append(details, fakeRunDetail(i, "success"))
	}
	details = append(details, fakeRunDetail(5, "failure"))
	attempt1 := fakeRunDetail(6, "failure")
	attempt2 := fakeRunDetail(6, "success")
	attempt2.Run.RunAttempt = 2
	attempt2.Run.UpdatedAt = attempt1.Run.UpdatedAt.Add(30 * time.Minute)
	details = append(details, attempt1, attempt2)

	runs := make([]model.WorkflowRun, len(details))
	for i := range details {
		runs[i] = details[i].Run
	}
	f := &fakeFetcher{
		workflows: []model.Workflow{{ID: 1, Name: "CI"}},
		runs:      map[int64][]model.WorkflowRun{1: runs},
		details:   details,
	}

	res, err := runService(t, f, nil)
	require.NoError(t, err)

	var failure *analyze.FailureDetail
	for _, finding := range res.Findings {
		if d, ok := finding.Detail.(analyze.FailureDetail); ok {
			failure = &d
			break
		}
	}
	require.NotNil(t, failure, "expected a failure finding")
	require.Positive(t, failure.RerunRate,
		"rerun stats computed from all attempts must reach the failure detail")
}

func TestRun_MissingRunnerLabelsAggregatedIntoOneDiagnostic(t *testing.T) {
	f := baseFetcher()
	for i := range f.details {
		f.details[i].Jobs[0].Labels = nil
	}

	res, err := runService(t, f, nil)
	require.NoError(t, err)

	var labelDiags int
	for _, d := range res.Diagnostics {
		if strings.Contains(d.Message, "runner labels unavailable") {
			labelDiags++
			require.Contains(t, d.Message, "2 jobs", "count must aggregate across all runs")
		}
	}
	require.Equal(t, 1, labelDiags, "one aggregated diagnostic, not one per job/workflow: %v", res.Diagnostics)
}

func TestRun_CancellationMidHydrationReturnsError(t *testing.T) {
	// Ctrl+C lands while a workflow is hydrating. Run must return the
	// context error — not a normal-looking analysis built from whatever
	// subset happened to hydrate before the cancel.
	f := baseFetcher()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancelDuringHydration = cancel

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	_, err := svc.Run(ctx, &Options{
		Repo:  "example-org/example-repo",
		Since: testBase.Add(-24 * time.Hour),
	})
	require.Error(t, err, "a cancelled run must not produce a normal-looking result")
	require.ErrorIs(t, err, context.Canceled)
}

func TestRun_TruncatedRunsAreNotCached(t *testing.T) {
	// A truncated detail (jobs/steps beyond the GraphQL per-query limit)
	// must be analyzed but never cached — a cached row would serve the
	// incomplete data forever.
	f := baseFetcher()
	f.details[1].Truncated = true

	st := &fakeStore{}
	_, err := runService(t, f, st)
	require.NoError(t, err)

	st.mu.Lock()
	defer st.mu.Unlock()
	savedIDs := make([]int64, 0, len(st.saved))
	for i := range st.saved {
		savedIDs = append(savedIDs, st.saved[i].Run.ID)
	}
	require.Contains(t, savedIDs, f.details[0].Run.ID, "complete run must be cached")
	require.NotContains(t, savedIDs, f.details[1].Run.ID, "truncated run must not be cached")
}

func TestRun_FilterContextRecordedInMeta(t *testing.T) {
	// A JSON/LLM consumer comparing two reports must be able to tell whether
	// one of them was filtered — otherwise a --branch main report and an
	// all-branches report look interchangeable.
	f := baseFetcher()

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	res, err := svc.Run(context.Background(), &Options{
		Repo:     "example-org/example-repo",
		Since:    testBase.Add(-24 * time.Hour),
		Branch:   "main",
		Workflow: "CI",
	})
	require.NoError(t, err)
	require.Equal(t, "main", res.Meta.Branch)
	require.Equal(t, "CI", res.Meta.Workflow)
	require.Equal(t, testBase.Add(-24*time.Hour), res.Meta.Since)
}

func TestRun_WorkflowFilterMatchingNothingListsAvailable(t *testing.T) {
	f := baseFetcher()
	f.workflows = append(f.workflows, model.Workflow{ID: 2, Name: "Deploy"})

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	_, err := svc.Run(context.Background(), &Options{
		Repo:     "example-org/example-repo",
		Since:    testBase.Add(-24 * time.Hour),
		Workflow: "Depoy", // typo
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), `"Depoy"`, "the typo'd filter must be named")
	require.Contains(t, err.Error(), "Deploy", "available workflow names help fix the typo")
	require.Contains(t, err.Error(), "CI")
}

func TestRun_NoRunsErrorMentionsWorkflowFilter(t *testing.T) {
	f := baseFetcher()
	f.runs = map[int64][]model.WorkflowRun{} // workflow exists, no runs

	svc := &Service{Client: f, Store: nil, Prog: output.NewProgress()}
	_, err := svc.Run(context.Background(), &Options{
		Repo:     "example-org/example-repo",
		Since:    testBase.Add(-24 * time.Hour),
		Workflow: "CI",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), `workflow "CI"`,
		"an active filter that produced zero runs must be visible in the error")
}

func TestRun_ListWorkflowsErrorNotDoubleWrapped(t *testing.T) {
	// The client already wraps its error with "list workflows:"; the service
	// must not add the same prefix again.
	f := baseFetcher()
	f.listWorkflowErr = errors.New(`list workflows: GET "https://api.github.com/...": 404`)

	_, err := runService(t, f, nil)
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(err.Error(), "list workflows:"),
		"error prefix duplicated: %v", err)
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
