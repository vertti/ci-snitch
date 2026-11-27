package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v93/github"
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

func TestFetchRunDetailsGraphQL_TruncatedRunsCompletedViaREST(t *testing.T) {
	// r0's checkRuns connection reports more pages (>50 jobs); r1 has a job
	// whose steps connection reports more pages. Both runs must be refetched
	// via REST (which paginates jobs fully and embeds complete steps) so the
	// final details are complete and cacheable; r2 keeps its GraphQL data.
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

	// REST serves the complete versions: two jobs each.
	for _, id := range []int64{1, 2} {
		body := fmt.Sprintf(`{"total_count": 2, "jobs": [
			{"id": %d, "run_id": %d, "name": "build", "status": "completed", "conclusion": "success"},
			{"id": %d, "run_id": %d, "name": "extra", "status": "completed", "conclusion": "success"}
		]}`, 1000+id, id, 2000+id, id)
		mux.HandleFunc(fmt.Sprintf("GET /repos/test-owner/test-repo/actions/runs/%d/jobs", id),
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
	}

	c := testClient(t, mux)
	runs := graphqlTestRuns(3)

	details, warnings := c.FetchRunDetailsGraphQL(context.Background(), runs)
	require.Len(t, details, 3)

	byID := map[int64]model.RunDetail{}
	for i := range details {
		byID[details[i].Run.ID] = details[i]
	}
	assert.Len(t, byID[1].Jobs, 2, "jobs-truncated run must be completed via REST")
	assert.False(t, byID[1].Truncated, "completed runs are cacheable")
	assert.Len(t, byID[2].Jobs, 2, "steps-truncated run must be completed via REST")
	assert.False(t, byID[2].Truncated)
	assert.Len(t, byID[3].Jobs, 1, "complete run keeps its GraphQL data")

	var notes []string
	for _, w := range warnings {
		if w.Kind == diag.KindPartialData {
			notes = append(notes, w.Message)
			assert.Equal(t, diag.Info, w.Severity, "completed runs are not partial data")
		}
	}
	require.Len(t, notes, 1, "one aggregated note, got %v", warnings)
	assert.Contains(t, notes[0], "2 runs")
	assert.Contains(t, notes[0], "REST")
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

func TestFetchRunDetailsGraphQL_UntypedRateLimitsDoNotFallBackToREST(t *testing.T) {
	// GitHub reports a rate limit in more shapes than the RATE_LIMITED type.
	// Each must stop hydration like the typed error, in GitHub's own words.
	cases := []struct {
		name       string
		status     int
		body       string
		wantReason string
	}{
		{
			name:       "error message without a type",
			status:     http.StatusOK,
			body:       `{"errors": [{"message": "API rate limit already exceeded for user ID 1."}]}`,
			wantReason: "API rate limit already exceeded for user ID 1.",
		},
		{
			name:       "secondary limit as 403",
			status:     http.StatusForbidden,
			body:       `{"documentation_url": "https://docs.github.com/graphql/overview/rate-limits-and-node-limits-for-the-graphql-api#secondary-rate-limits", "message": "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
			wantReason: "HTTP 403: You have exceeded a secondary rate limit.",
		},
		{
			name:       "429 with no body",
			status:     http.StatusTooManyRequests,
			wantReason: "HTTP 429",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var restCalls atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			mux.HandleFunc("GET /repos/test-owner/test-repo/actions/runs/{id}/jobs", func(w http.ResponseWriter, _ *http.Request) {
				restCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"total_count": 0, "jobs": []}`))
			})

			details, warnings := testClient(t, mux).FetchRunDetailsGraphQL(context.Background(), graphqlTestRuns(45))
			assert.Empty(t, details)
			assert.Zero(t, restCalls.Load(), "must not fall back to REST on a rate limit")

			require.Len(t, warnings, 1)
			assert.Equal(t, diag.KindRateLimit, warnings[0].Kind)
			assert.Contains(t, warnings[0].Message, "45 runs")
			assert.Contains(t, warnings[0].Message, tc.wantReason)
		})
	}
}

func TestFetchRunDetailsGraphQL_ForbiddenWithoutRateLimitStillFallsBackToREST(t *testing.T) {
	// A 403 is also how SSO and permission problems arrive; only the ones
	// that say "rate limit" are rate limits.
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Resource protected by organization SAML enforcement."}`))
	})
	restJobsHandler(mux, 1, 2)

	details, _ := testClient(t, mux).FetchRunDetailsGraphQL(context.Background(), graphqlTestRuns(2))
	assert.Len(t, details, 2)
}

func TestFetchRunDetailsGraphQL_StalledResponseTimesOutIntoRESTFallback(t *testing.T) {
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /graphql", func(http.ResponseWriter, *http.Request) {
		<-release
	})
	restJobsHandler(mux, 1, 2)

	c := testClient(t, mux, gh.WithTimeout(250*time.Millisecond))
	// Must run before the server's Close, which waits for the stalled handler.
	t.Cleanup(func() { close(release) })

	details, _ := c.FetchRunDetailsGraphQL(context.Background(), graphqlTestRuns(2))
	assert.Len(t, details, 2)
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

func TestParseGraphQLTime(t *testing.T) {
	ts := "2026-07-01T12:00:00Z"
	parsed := parseGraphQLTime(&ts)
	assert.Equal(t, 2026, parsed.Year())
	assert.Equal(t, 12, parsed.Hour())

	assert.True(t, parseGraphQLTime(nil).IsZero(), "nil timestamp is the zero time")
	empty := ""
	assert.True(t, parseGraphQLTime(&empty).IsZero())
	garbage := "not-a-time"
	assert.True(t, parseGraphQLTime(&garbage).IsZero(), "unparseable timestamps degrade to zero, not panic")
}

func TestGraphQLConclusion(t *testing.T) {
	success := "SUCCESS"
	assert.Equal(t, "success", graphqlConclusion(&success), "GraphQL enums are uppercase; REST uses lowercase")
	assert.Empty(t, graphqlConclusion(nil), "in-progress runs have no conclusion")
}

func TestConvertGraphQLJobs(t *testing.T) {
	started := "2026-07-01T12:00:00Z"
	completed := "2026-07-01T12:05:00Z"
	conclusion := "SUCCESS"
	checkRuns := []graphqlCheckRun{{
		Name: "build", DatabaseID: 42, Status: "COMPLETED", Conclusion: &conclusion,
		StartedAt: &started, CompletedAt: &completed,
	}}
	checkRuns[0].Steps.Nodes = []graphqlStep{{
		Name: "Checkout", Number: 1, Status: "COMPLETED", Conclusion: &conclusion,
		StartedAt: &started, CompletedAt: &started,
	}}

	jobs := convertGraphQLJobs(checkRuns, 7)
	require.Len(t, jobs, 1)
	assert.Equal(t, int64(42), jobs[0].ID)
	assert.Equal(t, int64(7), jobs[0].RunID, "jobs carry their parent run ID")
	assert.Equal(t, "completed", jobs[0].Status, "statuses normalized to REST's lowercase")
	assert.Equal(t, "success", jobs[0].Conclusion)
	require.Len(t, jobs[0].Steps, 1)
	assert.Equal(t, "Checkout", jobs[0].Steps[0].Name)
	assert.Equal(t, 1, jobs[0].Steps[0].Number)
}

func TestTruncateBody(t *testing.T) {
	assert.Equal(t, "short", truncateBody([]byte("short")))
	long := truncateBody([]byte(strings.Repeat("x", 300)))
	assert.Len(t, long, 203, "200 bytes plus ellipsis")
	assert.True(t, strings.HasSuffix(long, "..."))
}

func TestAPIErrorMessage(t *testing.T) {
	assert.Equal(t, "Bad credentials", string(apiErrorMessage([]byte(`{"message": "Bad credentials"}`))))
	assert.Equal(t, "<html>502</html>", string(apiErrorMessage([]byte("<html>502</html>"))), "a non-JSON body is shown as is")
	assert.JSONEq(t, `{"errors": []}`, string(apiErrorMessage([]byte(`{"errors": []}`))), "JSON without a message is shown as is")
	assert.Empty(t, apiErrorMessage(nil))
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

func TestFetchRunDetailsGraphQL_RateLimitWarningSaysWhenToRetry(t *testing.T) {
	reset := time.Date(2026, 3, 1, 10, 15, 0, 0, time.UTC)
	secondary := `{"message": "You have exceeded a secondary rate limit."}`
	typed := `{"errors": [{"type": "RATE_LIMITED", "message": "API rate limit exceeded"}]}`
	cases := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		check   func(t *testing.T, warning diag.Diagnostic)
	}{
		{
			name: "secondary limit with Retry-After", status: http.StatusForbidden,
			headers: map[string]string{"Retry-After": "60"}, body: secondary,
			check: func(t *testing.T, warning diag.Diagnostic) {
				assert.WithinDuration(t, time.Now().Add(time.Minute), warning.RetryAt, 5*time.Second)
				assert.Contains(t, warning.Message, "; retry after ")
			},
		},
		{
			name: "primary limit with an empty pool", status: http.StatusOK,
			headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(reset.Unix())}, body: typed,
			check: func(t *testing.T, warning diag.Diagnostic) {
				assert.True(t, reset.Equal(warning.RetryAt), "got %s", warning.RetryAt)
				assert.Contains(t, warning.Message, "; retry after 10:15:00 UTC")
			},
		},
		{
			name: "secondary limit that names no time, pool not empty", status: http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "4000", "X-RateLimit-Reset": fmt.Sprint(reset.Unix())}, body: secondary,
			check: func(t *testing.T, warning diag.Diagnostic) {
				assert.True(t, warning.RetryAt.IsZero(), "the primary reset says nothing about a secondary limit")
				assert.NotContains(t, warning.Message, "retry after")
			},
		},
		{
			name: "no headers", status: http.StatusOK, body: typed,
			check: func(t *testing.T, warning diag.Diagnostic) {
				assert.True(t, warning.RetryAt.IsZero())
				assert.NotContains(t, warning.Message, "retry after")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			_, warnings := testClient(t, mux).FetchRunDetailsGraphQL(context.Background(), graphqlTestRuns(3))
			require.Len(t, warnings, 1)
			require.Equal(t, diag.KindRateLimit, warnings[0].Kind)
			tc.check(t, warnings[0])
		})
	}
}
