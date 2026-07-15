package github

import (
	"context"
	"fmt"
	"net/http"
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
