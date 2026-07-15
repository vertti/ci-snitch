package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vertti/ci-snitch/internal/diag"
	"github.com/vertti/ci-snitch/internal/model"
)

func graphqlTestRuns(n int) []model.WorkflowRun {
	runs := make([]model.WorkflowRun, n)
	for i := range runs {
		id := int64(i + 1)
		runs[i] = model.WorkflowRun{
			ID: id, WorkflowID: 1, NodeID: fmt.Sprintf("node-%d", id),
			Status: "completed", Conclusion: "success",
		}
	}
	return runs
}

// restJobsHandler registers REST job endpoints for the given run IDs so the
// REST fallback path can hydrate them.
func restJobsHandler(mux *http.ServeMux, runIDs ...int64) {
	for _, id := range runIDs {
		body := fmt.Sprintf(`{"total_count": 1, "jobs": [{"id": %d, "run_id": %d, "name": "build", "status": "completed", "conclusion": "success"}]}`,
			1000+id, id)
		mux.HandleFunc(fmt.Sprintf("GET /repos/test-owner/test-repo/actions/runs/%d/jobs", id),
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
	}
}

func TestFetchRunDetailsGraphQL_MalformedBatchFallsBackToREST(t *testing.T) {
	// The GraphQL endpoint answers 200 with a data payload that is not the
	// expected object. Every run in the batch must still be hydrated (via
	// REST) — previously the whole batch vanished with zero diagnostics.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": "unexpected shape"}`))
	})
	restJobsHandler(mux, 1, 2)

	c := testClient(t, mux)
	runs := graphqlTestRuns(2)

	details, warnings := c.FetchRunDetailsGraphQL(context.Background(), runs)
	assert.Len(t, details, 2, "all runs must be hydrated via REST fallback")

	var partial int
	for _, w := range warnings {
		if w.Kind == diag.KindPartialData {
			partial++
		}
	}
	assert.Equal(t, 1, partial, "one aggregated partial-data warning expected, got %v", warnings)
}

func TestFetchRunDetailsGraphQL_MissingAliasFallsBackToREST(t *testing.T) {
	// The response accounts for r0 but omits r1 entirely (no key, not even
	// null). The missing run must be hydrated via REST with a warning —
	// previously it was silently dropped.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"r0": {
			"databaseId": 1,
			"checkSuite": {"checkRuns": {"nodes": [
				{"name": "build", "databaseId": 1001, "status": "COMPLETED", "conclusion": "SUCCESS",
				 "steps": {"nodes": []}}
			]}}
		}}}`))
	})
	restJobsHandler(mux, 2)

	c := testClient(t, mux)
	runs := graphqlTestRuns(2)

	details, warnings := c.FetchRunDetailsGraphQL(context.Background(), runs)
	require.Len(t, details, 2, "the run missing from the GraphQL response must be hydrated via REST")

	byID := map[int64][]model.Job{}
	for i := range details {
		byID[details[i].Run.ID] = details[i].Jobs
	}
	assert.Len(t, byID[1], 1, "run 1 comes from GraphQL")
	assert.Len(t, byID[2], 1, "run 2 comes from the REST fallback")

	var partial int
	for _, w := range warnings {
		if w.Kind == diag.KindPartialData {
			partial++
		}
	}
	assert.Equal(t, 1, partial, "one aggregated partial-data warning expected, got %v", warnings)
}

func TestFetchRunDetailsGraphQL_NoNodeIDsFallsBackToRESTSilently(t *testing.T) {
	// Runs without node IDs (e.g. loaded from an old cache) go straight to
	// REST — no GraphQL request, no spurious warnings.
	graphqlCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		graphqlCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	restJobsHandler(mux, 1, 2)

	c := testClient(t, mux)
	runs := graphqlTestRuns(2)
	for i := range runs {
		runs[i].NodeID = ""
	}

	details, warnings := c.FetchRunDetailsGraphQL(context.Background(), runs)
	assert.Len(t, details, 2)
	assert.Empty(t, warnings, "clean REST fallback must not produce warnings")
	assert.False(t, graphqlCalled, "no GraphQL request should be made when no run has a node ID")
}

func TestBuildBatchQuery_SelectsPageInfo(t *testing.T) {
	query := buildBatchQuery(graphqlTestRuns(1))
	require.Equal(t, 2, strings.Count(query, "pageInfo{hasNextPage}"),
		"both checkRuns and steps connections must expose truncation")
}

func TestBuildBatchQuery_FiltersLatestAttempt(t *testing.T) {
	// REST hydration requests filter=latest; without the matching GraphQL
	// filter, re-run runs carry duplicate old-attempt check runs and the two
	// hydration paths disagree on job/step stats.
	query := buildBatchQuery(graphqlTestRuns(1))
	require.Contains(t, query, "filterBy:{checkType:LATEST}",
		"checkRuns must fetch only the latest attempt, matching REST's filter=latest")
}

func TestDoGraphQL_ErrorBodyReadIsBounded(t *testing.T) {
	// A misconfigured proxy can answer with an arbitrarily large error page;
	// the error path must not slurp it into memory or into the error string.
	huge := strings.Repeat("x", 8<<20) // 8 MiB
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(huge))
	})

	c := testClient(t, mux)
	_, err := c.doGraphQL(context.Background(), "query{}")
	require.Error(t, err)
	require.Less(t, len(err.Error()), 500, "error string must stay truncated")
}

func TestFetchRunDetailsGraphQL_TruncationWarnsOnceAndMarksDetails(t *testing.T) {
	// r0's checkRuns connection reports more pages (>50 jobs); r1 has a job
	// whose steps connection reports more pages. Both runs must be marked
	// Truncated with ONE aggregated partial-data warning; r2 stays clean.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {
			"r0": {"databaseId": 1, "checkSuite": {"checkRuns": {
				"pageInfo": {"hasNextPage": true},
				"nodes": [{"name": "build", "databaseId": 1001, "status": "COMPLETED", "conclusion": "SUCCESS", "steps": {"nodes": []}}]
			}}},
			"r1": {"databaseId": 2, "checkSuite": {"checkRuns": {
				"pageInfo": {"hasNextPage": false},
				"nodes": [{"name": "build", "databaseId": 1002, "status": "COMPLETED", "conclusion": "SUCCESS",
					"steps": {"pageInfo": {"hasNextPage": true}, "nodes": []}}]
			}}},
			"r2": {"databaseId": 3, "checkSuite": {"checkRuns": {
				"pageInfo": {"hasNextPage": false},
				"nodes": [{"name": "build", "databaseId": 1003, "status": "COMPLETED", "conclusion": "SUCCESS", "steps": {"nodes": []}}]
			}}}
		}}`))
	})

	c := testClient(t, mux)
	runs := graphqlTestRuns(3)

	details, warnings := c.FetchRunDetailsGraphQL(context.Background(), runs)
	require.Len(t, details, 3)

	truncatedByID := map[int64]bool{}
	for i := range details {
		truncatedByID[details[i].Run.ID] = details[i].Truncated
	}
	assert.True(t, truncatedByID[1], "jobs-truncated run must be marked")
	assert.True(t, truncatedByID[2], "steps-truncated run must be marked")
	assert.False(t, truncatedByID[3], "complete run must not be marked")

	var truncationWarnings []string
	for _, w := range warnings {
		if w.Kind == diag.KindPartialData {
			truncationWarnings = append(truncationWarnings, w.Message)
		}
	}
	require.Len(t, truncationWarnings, 1, "one aggregated truncation warning, got %v", warnings)
	assert.Contains(t, truncationWarnings[0], "2 runs")
}

func TestFetchRunDetailsGraphQL_RateLimitedDoesNotFallBackToREST(t *testing.T) {
	// When the GraphQL pool is exhausted, falling back to REST would burn
	// ~20x the approved core budget. Skip hydration with a warning instead.
	restCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded for user"}]}`))
	})
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/runs/{id}/jobs", func(w http.ResponseWriter, _ *http.Request) {
		restCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count": 0, "jobs": []}`))
	})

	c := testClient(t, mux)
	runs := graphqlTestRuns(45) // 3 batches — must stop after the first hits the limit

	details, warnings := c.FetchRunDetailsGraphQL(context.Background(), runs)
	assert.Empty(t, details)
	assert.Equal(t, 0, restCalls, "must not fall back to REST on GraphQL rate limiting")

	var rateWarnings int
	for _, w := range warnings {
		if w.Kind == diag.KindRateLimit {
			rateWarnings++
			assert.Contains(t, w.Message, "45 runs", "warning must cover all skipped runs")
		}
	}
	assert.Equal(t, 1, rateWarnings, "one aggregated rate-limit warning, got %v", warnings)
}

func TestFetchRunDetailsGraphQL_CancelledContextAbortsCleanly(t *testing.T) {
	// A cancelled context is not a data problem: no REST fallback (it would
	// fail identically) and no per-run warning spam — the caller sees the
	// context error through ctx.Err().
	restCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /repos/test-owner/test-repo/actions/runs/{id}/jobs", func(w http.ResponseWriter, _ *http.Request) {
		restCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_count": 0, "jobs": []}`))
	})

	c := testClient(t, mux)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate Ctrl+C before/mid hydration

	details, warnings := c.FetchRunDetailsGraphQL(ctx, graphqlTestRuns(45))
	assert.Empty(t, details)
	assert.Equal(t, 0, restCalls, "no REST fallback on a cancelled context")
	assert.Empty(t, warnings, "cancellation must not masquerade as per-run fetch failures: %v", warnings)
}

func TestFetchRunDetailsGraphQL_NullNodeStillWarns(t *testing.T) {
	// A "rN": null node (deleted run / permissions) keeps its per-run
	// warning — pins the pre-existing behavior.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data": {"r0": null}}`))
	})

	c := testClient(t, mux)
	runs := graphqlTestRuns(1)

	details, warnings := c.FetchRunDetailsGraphQL(context.Background(), runs)
	assert.Empty(t, details)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "failed to parse")
}
